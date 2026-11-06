package server

import (
	"context"
	"strconv"
	"strings"
	"unicode"
)

// searchCaps are the properties Search can match on.
const searchCaps = "dc:title,upnp:class,dc:description,upnp:channelName,upnp:channelNr"

// search answers the ContentDirectory Search action: the items, and folders,
// under ContainerID that fit SearchCriteria, in the shape of a Browse.
func (s *Server) search(ctx context.Context, v view, args map[string]string) ([][2]string, error) {
	start, err1 := atoiOr(args["StartingIndex"], 0)
	count, err2 := atoiOr(args["RequestedCount"], 0)
	if err1 != nil || err2 != nil || start < 0 || count < 0 {
		return nil, errInvalidArgs
	}
	crit, err := parseCriteria(args["SearchCriteria"])
	if err != nil {
		return nil, &soapError{708, "Unsupported or invalid search criteria"}
	}
	id := args["ContainerID"]
	if id == "" {
		id = idRoot
	}
	if _, err := s.lookup(ctx, v, id); err != nil {
		return nil, &soapError{710, "No such container"}
	}
	var all []object
	if err := s.walk(ctx, v, id, func(o object) {
		if crit(o) {
			all = append(all, o)
		}
	}); err != nil {
		return nil, err
	}
	total := len(all)
	all = all[min(start, len(all)):]
	if count > 0 && count < len(all) {
		all = all[:count]
	}
	return [][2]string{
		{"Result", didl(all)},
		{"NumberReturned", strconv.Itoa(len(all))},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", s.systemUpdateID()},
	}, nil
}

// walk calls fn for everything under a container, depth first. From the top it
// goes only through the folders that hold each thing once: a channel is in Live
// TV and maybe Favorites and its source, a recording in its show and maybe
// Recent; it is found once, in Live TV, TV Shows or Movies. An episode is
// titled with its show, as in Recent recordings, so that a search for the show
// finds it and its title says what it is.
func (s *Server) walk(ctx context.Context, v view, id string, fn func(object)) error {
	show := ""
	if strings.HasPrefix(id, "recgroup/") {
		if f, err := s.lookup(ctx, v, id); err == nil {
			show = f.Title
		}
	}
	kids, err := s.children(ctx, v, id)
	if err != nil {
		return err
	}
	for _, o := range kids {
		if id == idRoot && o.ID != idLive && o.ID != idRecordings && o.ID != idMovies {
			continue
		}
		if show != "" && !o.Folder && !strings.HasPrefix(o.Title, show) {
			o.Title = show + " - " + o.Title
		}
		fn(o)
		if o.Folder {
			if err := s.walk(ctx, v, o.ID, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// A criterion says whether an object fits.
type criterion func(object) bool

// property is an object's value for a search property, and whether it has one.
func property(o object, name string) (string, bool) {
	switch name {
	case "dc:title":
		return o.Title, true
	case "upnp:class":
		if o.Folder {
			return "object.container.storageFolder", true
		}
		return o.Class, true
	case "dc:description":
		return o.Description, o.Description != ""
	case "upnp:channelName":
		return o.ChannelName, o.ChannelName != ""
	case "upnp:channelNr":
		return o.ChannelNr, o.ChannelNr != ""
	case "@id":
		return o.ID, true
	case "@parentID":
		return o.Parent, true
	}
	return "", false
}

// parseCriteria reads a UPnP search criteria string: "*", or comparisons such
// as dc:title contains "news" or upnp:class derivedfrom "object.item.videoItem",
// joined by and and or, in brackets as needed. and binds tighter than or.
func parseCriteria(s string) (criterion, error) {
	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	if len(toks) == 1 && toks[0].text == "*" && !toks[0].quoted {
		return func(object) bool { return true }, nil
	}
	p := &critParser{toks: toks}
	c, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.toks) {
		return nil, errBadCriteria
	}
	return c, nil
}

type token struct {
	text   string
	quoted bool
}

type critError string

func (e critError) Error() string { return string(e) }

const errBadCriteria = critError("bad search criteria")

func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(' || c == ')':
			toks = append(toks, token{text: string(c)})
			i++
		case c == '"':
			var b strings.Builder
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errBadCriteria // unterminated
			}
			i++
			toks = append(toks, token{text: b.String(), quoted: true})
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && s[j] != '(' && s[j] != ')' && s[j] != '"' {
				j++
			}
			toks = append(toks, token{text: s[i:j]})
			i = j
		}
	}
	if len(toks) == 0 {
		return nil, errBadCriteria
	}
	return toks, nil
}

type critParser struct {
	toks []token
	i    int
}

func (p *critParser) peek(word string) bool {
	return p.i < len(p.toks) && !p.toks[p.i].quoted && strings.EqualFold(p.toks[p.i].text, word)
}

func (p *critParser) or() (criterion, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.peek("or") {
		p.i++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		a, b := l, r
		l = func(o object) bool { return a(o) || b(o) }
	}
	return l, nil
}

func (p *critParser) and() (criterion, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	for p.peek("and") {
		p.i++
		r, err := p.term()
		if err != nil {
			return nil, err
		}
		a, b := l, r
		l = func(o object) bool { return a(o) && b(o) }
	}
	return l, nil
}

func (p *critParser) term() (criterion, error) {
	if p.peek("(") {
		p.i++
		c, err := p.or()
		if err != nil || !p.peek(")") {
			return nil, errBadCriteria
		}
		p.i++
		return c, nil
	}
	if p.i+3 > len(p.toks) {
		return nil, errBadCriteria
	}
	prop, op, val := p.toks[p.i], p.toks[p.i+1], p.toks[p.i+2]
	if prop.quoted || op.quoted {
		return nil, errBadCriteria
	}
	p.i += 3
	name, want := prop.text, val.text
	lw := strings.ToLower(want)
	switch strings.ToLower(op.text) {
	case "exists":
		if val.quoted {
			return nil, errBadCriteria
		}
		exists := strings.EqualFold(want, "true")
		return func(o object) bool { _, ok := property(o, name); return ok == exists }, nil
	}
	if !val.quoted {
		return nil, errBadCriteria
	}
	switch strings.ToLower(op.text) {
	case "=":
		return func(o object) bool { v, ok := property(o, name); return ok && strings.EqualFold(v, want) }, nil
	case "!=":
		return func(o object) bool { v, ok := property(o, name); return !ok || !strings.EqualFold(v, want) }, nil
	case "contains":
		return func(o object) bool { v, ok := property(o, name); return ok && strings.Contains(strings.ToLower(v), lw) }, nil
	case "doesnotcontain":
		return func(o object) bool {
			v, ok := property(o, name)
			return !ok || !strings.Contains(strings.ToLower(v), lw)
		}, nil
	case "startswith":
		return func(o object) bool {
			v, ok := property(o, name)
			return ok && strings.HasPrefix(strings.ToLower(v), lw)
		}, nil
	case "derivedfrom":
		return func(o object) bool {
			v, ok := property(o, name)
			v = strings.ToLower(v)
			return ok && (v == lw || strings.HasPrefix(v, lw+"."))
		}, nil
	case "<", "<=", ">", ">=":
		cmp := op.text
		return func(o object) bool {
			v, ok := property(o, name)
			if !ok {
				return false
			}
			c := strings.Compare(strings.ToLower(v), lw)
			switch cmp {
			case "<":
				return c < 0
			case "<=":
				return c <= 0
			case ">":
				return c > 0
			}
			return c >= 0
		}, nil
	}
	return nil, errBadCriteria
}
