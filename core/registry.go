package core

import "fmt"

var registry = map[string]Provider{}

// Register makes a provider available by name. Provider packages call this
// from an init(), so a blank import (e.g. `_ "iko-dbsnap/providers/postgres"`)
// is all main.go needs to make a new engine selectable.
func Register(p Provider) {
	registry[p.Name()] = p
}

// Get looks up a registered provider by name.
func Get(name string) (Provider, error) {
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown db provider %q (available: %v)", name, Names())
	}
	return p, nil
}

// Names lists all registered provider names.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	return names
}
