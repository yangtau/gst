package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// legacyFile is the configuration of gst 0.2 and earlier: tunnels with
// forwards in the manner of ssh -L, -R, and -D.
type legacyFile struct {
	Mesh    *Node                    `json:"mesh"`
	Tunnels map[string]*legacyTunnel `json:"tunnels"`
}

type legacyTunnel struct {
	Link
	Forwards []struct {
		Direction string `json:"direction"`
		Listen    string `json:"listen"`
		Target    string `json:"target"`
	} `json:"forwards"`
}

// parseLegacy converts a configuration of an earlier version: the mesh
// identity becomes the node; each SSH tunnel becomes a link of the same
// name; and each -L forward becomes a map to the tunnel's machine, which
// for a tunnel to mesh:<machine> is that machine. Other forwards have no
// place in the current format and are dropped, with a note each. The
// notes are not nil.
func parseLegacy(data []byte) (*File, []string, error) {
	var old legacyFile
	if err := json.Unmarshal(data, &old); err != nil {
		return nil, nil, errors.New("invalid JSON")
	}
	f := empty()
	if old.Mesh != nil {
		f.Node = &Node{ID: old.Mesh.ID, Name: old.Mesh.Name}
	}
	notes := []string{}
	for _, name := range slices.Sorted(maps.Keys(old.Tunnels)) {
		t := old.Tunnels[name]
		if t == nil {
			continue
		}
		machine, through := strings.CutPrefix(t.Destination, "mesh:")
		if !through {
			l := t.Link
			f.Links[name] = &l
			machine = name
		}
		for _, fwd := range t.Forwards {
			m := Map{Listen: fwd.Listen, Machine: machine, Target: fwd.Target}
			if fwd.Direction == "local" && m.validate() == nil && !slices.ContainsFunc(f.Maps, func(e Map) bool { return e.Listen == m.Listen }) {
				f.Maps = append(f.Maps, m)
				continue
			}
			flag := map[string]string{"remote": "-R", "local": "-L", "dynamic": "-D"}[fwd.Direction]
			spec := strings.TrimSuffix(fwd.Listen+":"+fwd.Target, ":")
			notes = append(notes, fmt.Sprintf("tunnel %s: %s %s", name, flag, spec))
		}
	}
	if err := f.normalize(); err != nil {
		return nil, nil, err
	}
	return f, notes, nil
}
