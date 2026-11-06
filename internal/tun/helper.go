package tun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"reflect"
	"syscall"

	"golang.org/x/sys/unix"
	wgtun "golang.zx2c4.com/wireguard/tun"

	"github.com/yangtau/ssh-tunnel/internal/wire"
)

// Helper is the service that sets up the interface. It runs as root.
type Helper struct {
	Socket   string
	UID, GID int // the user whose daemon it serves
	Log      *log.Logger

	sys system // this machine's, when nil
}

// Serve answers the daemon until ctx is done. A daemon that connects
// replaces the one before it, which is gone or about to be.
func (h *Helper) Serve(ctx context.Context) error {
	if h.sys == nil {
		h.sys = newSystem()
	}
	if err := os.Remove(h.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	old := syscall.Umask(0o177)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: h.Socket, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chown(h.Socket, h.UID, h.GID); err != nil {
		return err
	}
	defer context.AfterFunc(ctx, func() { listener.Close() })()
	var current *session
	defer func() {
		if current != nil {
			current.end()
		}
	}()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if current != nil {
			current.end()
		}
		current = &session{helper: h, conn: conn, done: make(chan struct{})}
		go current.serve()
	}
}

// session is one daemon's interface.
type session struct {
	helper *Helper
	conn   *net.UnixConn
	done   chan struct{}

	file    *os.File
	name    string
	current *parsed // what the system is set to
}

// end stops the session and waits until it took everything down.
func (s *session) end() {
	s.conn.Close()
	<-s.done
}

func (s *session) serve() {
	defer close(s.done)
	defer s.conn.Close()
	defer s.down()
	for {
		var spec Spec
		if err := wire.ReadFrame(s.conn, &spec); err != nil {
			return
		}
		first := s.file == nil
		reply := Reply{}
		if err := s.apply(spec); err != nil {
			reply.Error = err.Error()
			// Errors go to the daemon, and the log leaves out what they name.
			s.helper.Log.Print("a request failed")
		} else {
			reply.Interface = s.name
		}
		if err := wire.WriteFrame(s.conn, reply); err != nil {
			return
		}
		if first && s.file != nil && reply.Error == "" {
			if err := s.sendDevice(); err != nil {
				return
			}
		}
	}
}

// sendDevice hands the daemon the open device.
func (s *session) sendDevice() error {
	rights := unix.UnixRights(int(s.file.Fd()))
	_, _, err := s.conn.WriteMsgUnix([]byte{0}, rights, nil)
	return err
}

// apply brings the system to spec.
func (s *session) apply(spec Spec) error {
	want, err := spec.parse()
	if err != nil {
		return err
	}
	if s.current != nil && reflect.DeepEqual(*s.current, want) {
		return nil
	}
	sys := s.helper.sys
	if s.file == nil {
		if s.file, s.name, err = sys.open(); err != nil {
			return err
		}
		s.helper.Log.Printf("%s created", s.name)
	}
	if s.current == nil || s.current.address != want.address || s.current.subnet != want.subnet {
		if s.current != nil {
			sys.unnumber(s.name, s.current.address, s.current.subnet)
		}
		s.current = nil
		if err := sys.number(s.name, want.address, want.subnet); err != nil {
			return err
		}
	}
	s.current = &parsed{subnet: want.subnet, address: want.address}
	if err := sys.resolve(want.hosts); err != nil {
		return fmt.Errorf("set up name resolution: %w", err)
	}
	s.current.hosts = want.hosts
	return nil
}

// down takes down what the session set up. The interface goes with the
// last open file of its device, the daemon's.
func (s *session) down() {
	if s.file == nil {
		return
	}
	sys := s.helper.sys
	sys.unresolve()
	if s.current != nil {
		sys.unnumber(s.name, s.current.address, s.current.subnet)
	}
	s.file.Close()
	s.helper.Log.Printf("%s removed", s.name)
}

// Client is the daemon's connection to the helper. The interface lives
// until Close.
type Client struct {
	conn   *net.UnixConn
	device *Device
}

// ErrNoHelper says that the helper does not run.
var ErrNoHelper = errors.New("the gst-net helper does not run")

// Dial asks the helper at socket for an interface as spec says.
func Dial(socket string, spec Spec) (*Client, error) {
	raw, err := net.Dial("unix", socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNoHelper
		}
		return nil, err
	}
	c := &Client{conn: raw.(*net.UnixConn)}
	reply, err := c.Set(spec)
	if err != nil {
		raw.Close()
		return nil, err
	}
	file, err := c.receiveFile()
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("receive the device: %w", err)
	}
	c.device = newDevice(file, reply)
	return c, nil
}

func (c *Client) receiveFile() (*os.File, error) {
	var data [1]byte
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := c.conn.ReadMsgUnix(data[:], oob)
	if err != nil {
		return nil, err
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(messages) != 1 {
		return nil, errors.New("no device in the message")
	}
	fds, err := unix.ParseUnixRights(&messages[0])
	if err != nil || len(fds) != 1 {
		return nil, errors.New("no device in the message")
	}
	unix.CloseOnExec(fds[0])
	// In non-blocking mode the runtime polls it, so that closing it ends
	// the read in progress.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		unix.Close(fds[0])
		return nil, err
	}
	return os.NewFile(uintptr(fds[0]), "tun"), nil
}

// Set brings the interface to spec, and returns its name.
func (c *Client) Set(spec Spec) (string, error) {
	if err := wire.WriteFrame(c.conn, spec); err != nil {
		return "", err
	}
	var reply Reply
	if err := wire.ReadFrame(c.conn, &reply); err != nil {
		return "", err
	}
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	return reply.Interface, nil
}

// Device returns the interface's device.
func (c *Client) Device() wgtun.Device { return c.device }

// Close ends the connection, which takes down the interface once its
// device is closed too.
func (c *Client) Close() error { return c.conn.Close() }
