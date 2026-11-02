// Package ssdp announces the media server on the LAN and answers TVs that
// search for one.
package ssdp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var group = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

const maxAge = 1800

// Types are the device and service types the server offers, besides
// upnp:rootdevice and its uuid.
var Types = []string{
	"urn:schemas-upnp-org:device:MediaServer:1",
	"urn:schemas-upnp-org:service:ContentDirectory:1",
	"urn:schemas-upnp-org:service:ConnectionManager:1",
}

type Advertiser struct {
	UUID   string // without the uuid: prefix
	Port   int    // of the HTTP server
	Server string // SERVER header
	// Host, when set, is the address put in LOCATION. Otherwise each answer
	// carries the local address facing the TV that asked.
	Host  string
	Iface *net.Interface // nil: the system's default multicast interface
	Logf  func(format string, a ...any)
}

// Run answers searches and sends alive notices until ctx ends, then says
// byebye.
func (a *Advertiser) Run(ctx context.Context) error {
	conn, err := net.ListenMulticastUDP("udp4", a.Iface, group)
	if err != nil {
		return fmt.Errorf("ssdp: %w", err)
	}
	go func() {
		a.notifyAll(conn, "ssdp:alive")
		// UDP gets lost; repeat the first notice once soon after start.
		soon := time.After(3 * time.Second)
		t := time.NewTicker(maxAge / 3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				a.notifyAll(conn, "ssdp:byebye")
				conn.Close()
				return
			case <-soon:
				a.notifyAll(conn, "ssdp:alive")
			case <-t.C:
				a.notifyAll(conn, "ssdp:alive")
			}
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("ssdp: %w", err)
		}
		st, mx, ok := parseSearch(buf[:n])
		if !ok {
			continue
		}
		answers := a.answers(st)
		if len(answers) == 0 {
			continue
		}
		go a.reply(conn, from, answers, mx)
	}
}

// parseSearch reads an M-SEARCH and returns its search target and MX.
func parseSearch(b []byte) (st string, mx int, ok bool) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b)))
	if err != nil || req.Method != "M-SEARCH" {
		return "", 0, false
	}
	if strings.Trim(req.Header.Get("Man"), `"`) != "ssdp:discover" {
		return "", 0, false
	}
	mx, _ = strconv.Atoi(req.Header.Get("Mx"))
	return req.Header.Get("St"), mx, true
}

type target struct{ nt, usn string }

func (a *Advertiser) targets() []target {
	uuid := "uuid:" + a.UUID
	ts := []target{{"upnp:rootdevice", uuid + "::upnp:rootdevice"}, {uuid, uuid}}
	for _, t := range Types {
		ts = append(ts, target{t, uuid + "::" + t})
	}
	return ts
}

func (a *Advertiser) answers(st string) []target {
	if st == "ssdp:all" {
		return a.targets()
	}
	for _, t := range a.targets() {
		if t.nt == st {
			return []target{t}
		}
	}
	return nil
}

func (a *Advertiser) reply(conn *net.UDPConn, to *net.UDPAddr, ts []target, mx int) {
	// Spread answers over part of MX, as the spec asks, but keep it short:
	// some TVs give up after a second.
	if mx > 0 {
		time.Sleep(time.Duration(rand.IntN(min(mx, 1)*500+1)) * time.Millisecond)
	}
	loc := a.location(to)
	for _, t := range ts {
		msg := "HTTP/1.1 200 OK\r\n" +
			"CACHE-CONTROL: max-age=" + strconv.Itoa(maxAge) + "\r\n" +
			"DATE: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n" +
			"EXT:\r\n" +
			"LOCATION: " + loc + "\r\n" +
			"SERVER: " + a.Server + "\r\n" +
			"ST: " + t.nt + "\r\n" +
			"USN: " + t.usn + "\r\n" +
			"Content-Length: 0\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), to); err != nil {
			a.logf("ssdp: answer to %s: %v", to, err)
			return
		}
	}
}

func (a *Advertiser) notifyAll(conn *net.UDPConn, nts string) {
	loc := a.location(group)
	for _, t := range a.targets() {
		msg := "NOTIFY * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"NT: " + t.nt + "\r\n" +
			"NTS: " + nts + "\r\n" +
			"USN: " + t.usn + "\r\n"
		if nts == "ssdp:alive" {
			msg += "CACHE-CONTROL: max-age=" + strconv.Itoa(maxAge) + "\r\n" +
				"LOCATION: " + loc + "\r\n" +
				"SERVER: " + a.Server + "\r\n"
		}
		msg += "\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), group); err != nil {
			a.logf("ssdp: %s: %v", nts, err)
			return
		}
	}
}

func (a *Advertiser) location(peer *net.UDPAddr) string {
	host := a.Host
	if host == "" {
		host = a.localIP(peer)
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(a.Port)) + "/description.xml"
}

// localIP is the address of this machine that peer reaches it on.
func (a *Advertiser) localIP(peer *net.UDPAddr) string {
	if a.Iface != nil && peer.IP.IsMulticast() {
		if addrs, err := a.Iface.Addrs(); err == nil {
			for _, ad := range addrs {
				if ip, ok := ad.(*net.IPNet); ok && ip.IP.To4() != nil {
					return ip.IP.String()
				}
			}
		}
	}
	c, err := net.DialUDP("udp4", nil, peer)
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func (a *Advertiser) logf(format string, v ...any) {
	if a.Logf != nil {
		a.Logf(format, v...)
	}
}
