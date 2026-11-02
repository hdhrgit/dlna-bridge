package server

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// action is one SOAP call: its name and its arguments by name.
type action struct {
	Name string
	Args map[string]string
}

// readAction finds the action element inside the SOAP body.
func readAction(r io.Reader) (action, error) {
	d := xml.NewDecoder(io.LimitReader(r, 64<<10))
	a := action{Args: map[string]string{}}
	depth, inBody := 0, false
	var arg string
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return a, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case !inBody && t.Name.Local == "Body":
				inBody = true
				depth = 0
			case inBody && depth == 1:
				a.Name = t.Name.Local
			case inBody && depth == 2:
				arg = t.Name.Local
				a.Args[arg] = ""
			}
		case xml.CharData:
			if inBody && depth == 2 && arg != "" {
				a.Args[arg] += string(t)
			}
		case xml.EndElement:
			if inBody && depth == 2 {
				arg = ""
			}
			depth--
		}
	}
	if a.Name == "" {
		return a, fmt.Errorf("no action in the SOAP body")
	}
	return a, nil
}

// soapError is a UPnP error code for a fault.
type soapError struct {
	Code int
	Desc string
}

func (e *soapError) Error() string { return fmt.Sprintf("UPnP error %d: %s", e.Code, e.Desc) }

var (
	errInvalidAction = &soapError{401, "Invalid Action"}
	errInvalidArgs   = &soapError{402, "Invalid Args"}
	errNoSuchObject  = &soapError{701, "No such object"}
)

// writeResponse sends the out arguments of a call, in order.
func writeResponse(w http.ResponseWriter, service, name string, out [][2]string) {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%sResponse xmlns:u="%s">`, name, service)
	for _, kv := range out {
		b.WriteString("<" + kv[0] + ">")
		xml.EscapeText(&b, []byte(kv[1]))
		b.WriteString("</" + kv[0] + ">")
	}
	fmt.Fprintf(&b, `</u:%sResponse></s:Body></s:Envelope>`, name)
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Ext", "")
	w.Write(b.Bytes())
}

func writeFault(w http.ResponseWriter, e *soapError) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, e.Code, escape(e.Desc))
}

func escape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
