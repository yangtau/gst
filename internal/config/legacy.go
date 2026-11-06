package config

import (
	"encoding/json"
	"errors"
	"fmt"
)

// legacyVersion is the configuration format that gst still converts: that
// of gst 0.3, with a proxy, maps to machines' ports and sockets, and routes.
const legacyVersion = 3

// legacyFile is a configuration of legacyVersion.
type legacyFile struct {
	Node *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node"`
	Down  bool             `json:"down"`
	Proxy string           `json:"proxy"`
	Links map[string]*Link `json:"links"`
	Maps  []struct {
		Listen  string `json:"listen"`
		Machine string `json:"machine"`
		Target  string `json:"target"`
	} `json:"maps"`
	Routes []struct {
		Match string `json:"match"`
		Via   string `json:"via"`
	} `json:"routes"`
	Net *Net `json:"net"`
}

// parseLegacy converts a configuration of legacyVersion. Its identity and
// links stay; the proxy, the maps, and the routes have no place in the
// current format and go, with a note each. The notes are not nil.
func parseLegacy(data []byte) (*File, []string, error) {
	var old legacyFile
	if err := json.Unmarshal(data, &old); err != nil {
		return nil, nil, errors.New("invalid JSON")
	}
	f := empty()
	f.Down = old.Down
	f.Links = old.Links
	if old.Node != nil {
		f.Node = &Node{ID: old.Node.ID, Name: old.Node.Name}
	}
	if old.Net != nil && old.Net.Subnet != "" {
		f.Net = old.Net
	}
	notes := []string{}
	if old.Proxy != "" {
		notes = append(notes, "proxy "+old.Proxy)
	}
	for _, m := range old.Maps {
		notes = append(notes, fmt.Sprintf("map %s to %s on %s", m.Listen, m.Target, m.Machine))
	}
	for _, r := range old.Routes {
		notes = append(notes, fmt.Sprintf("route %s via %s", r.Match, r.Via))
	}
	if err := f.normalize(); err != nil {
		return nil, nil, err
	}
	return f, notes, nil
}
