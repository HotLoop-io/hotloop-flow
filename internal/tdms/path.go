package tdms

import (
	"errors"
	"strings"
)

// Object paths are /, /'group' and /'group'/'channel'. A quote inside a name is
// written twice.

const (
	kindFile = iota
	kindGroup
	kindChannel
)

func quoteName(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func groupPath(g string) string { return "/" + quoteName(g) }

func channelPath(g, c string) string { return "/" + quoteName(g) + "/" + quoteName(c) }

var errBadPath = errors.New("bad object path")

func parsePath(p string) (kind int, group, channel string, err error) {
	if p == "/" {
		return kindFile, "", "", nil
	}
	rest := p
	var names []string
	for rest != "" {
		if rest[0] != '/' || len(rest) < 2 || rest[1] != '\'' {
			return 0, "", "", errBadPath
		}
		rest = rest[2:]
		var name strings.Builder
		closed := false
		for len(rest) > 0 {
			c := rest[0]
			if c == '\'' {
				if len(rest) > 1 && rest[1] == '\'' {
					name.WriteByte('\'')
					rest = rest[2:]
					continue
				}
				rest = rest[1:]
				closed = true
				break
			}
			name.WriteByte(c)
			rest = rest[1:]
		}
		if !closed {
			return 0, "", "", errBadPath
		}
		names = append(names, name.String())
		if len(names) > 2 {
			return 0, "", "", errBadPath
		}
	}
	switch len(names) {
	case 1:
		return kindGroup, names[0], "", nil
	case 2:
		return kindChannel, names[0], names[1], nil
	}
	return 0, "", "", errBadPath
}
