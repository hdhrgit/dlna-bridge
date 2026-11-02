package ssdp

import "testing"

func TestSearch(t *testing.T) {
	st, mx, ok := parseSearch([]byte("M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n"))
	if !ok || mx != 2 || st != "urn:schemas-upnp-org:device:MediaServer:1" {
		t.Fatalf("parse: %q %d %v", st, mx, ok)
	}
	if _, _, ok := parseSearch([]byte("NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\n\r\n")); ok {
		t.Error("a NOTIFY is not a search")
	}
	a := &Advertiser{UUID: "abc"}
	if got := a.answers(st); len(got) != 1 || got[0].usn != "uuid:abc::"+st {
		t.Errorf("answers: %+v", got)
	}
	if got := a.answers("ssdp:all"); len(got) != 5 {
		t.Errorf("ssdp:all: %d answers", len(got))
	}
	if got := a.answers("urn:schemas-upnp-org:device:MediaRenderer:1"); got != nil {
		t.Errorf("renderer search answered: %+v", got)
	}
}
