package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

const (
	typeA    = 1
	typeAAAA = 28
	typeANY  = 255

	rcodeFormErr  = 1
	rcodeNotImp   = 4
	rcodeRefused  = 5
	dnsAnswerTTL  = 60
	maxUDPMessage = 1232
)

type dnsServer struct {
	domain    string
	iface     string
	fixedIPv4 net.IP
	fixedIPv6 net.IP
}

func (d *dnsServer) addresses() (v4, v6 net.IP) {
	v4, v6 = d.fixedIPv4, d.fixedIPv6
	if d.iface == "" {
		return
	}
	ifc, err := net.InterfaceByName(d.iface)
	if err != nil {
		return
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		if ip4 := ip.To4(); ip4 != nil {
			if v4 == nil {
				v4 = ip4
			}
		} else if v6 == nil && ip.IsGlobalUnicast() {
			v6 = ip
		}
	}
	return
}

func (d *dnsServer) inZone(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	return name == d.domain || strings.HasSuffix(name, "."+d.domain)
}

func (d *dnsServer) answer(req []byte) []byte {
	if len(req) < 12 {
		return nil
	}
	id := binary.BigEndian.Uint16(req[0:2])
	flags := binary.BigEndian.Uint16(req[2:4])
	if flags&0x8000 != 0 {
		return nil
	}
	opcode := flags & 0x7800
	respFlags := 0x8000 | opcode | 0x0400 | (flags & 0x0100)

	reply := func(rcode uint16, question []byte, answers [][]byte) []byte {
		out := make([]byte, 12, 12+len(question)+64)
		binary.BigEndian.PutUint16(out[0:2], id)
		binary.BigEndian.PutUint16(out[2:4], respFlags|rcode)
		if question != nil {
			binary.BigEndian.PutUint16(out[4:6], 1)
		}
		binary.BigEndian.PutUint16(out[6:8], uint16(len(answers)))
		out = append(out, question...)
		for _, a := range answers {
			out = append(out, a...)
		}
		return out
	}

	if opcode != 0 {
		return reply(rcodeNotImp, nil, nil)
	}
	if binary.BigEndian.Uint16(req[4:6]) != 1 {
		return reply(rcodeFormErr, nil, nil)
	}
	name, end, err := readName(req, 12)
	if err != nil || end+4 > len(req) {
		return reply(rcodeFormErr, nil, nil)
	}
	question := req[12 : end+4]
	qtype := binary.BigEndian.Uint16(req[end : end+2])
	if !d.inZone(name) {
		return reply(rcodeRefused, question, nil)
	}

	v4, v6 := d.addresses()
	var answers [][]byte
	if v4 != nil && (qtype == typeA || qtype == typeANY) {
		answers = append(answers, record(typeA, v4.To4()))
	}
	if v6 != nil && (qtype == typeAAAA || qtype == typeANY) {
		answers = append(answers, record(typeAAAA, v6.To16()))
	}
	return reply(0, question, answers)
}

func record(rtype uint16, data []byte) []byte {
	rr := make([]byte, 12, 12+len(data))
	binary.BigEndian.PutUint16(rr[0:2], 0xC00C)
	binary.BigEndian.PutUint16(rr[2:4], rtype)
	binary.BigEndian.PutUint16(rr[4:6], 1)
	binary.BigEndian.PutUint32(rr[6:10], dnsAnswerTTL)
	binary.BigEndian.PutUint16(rr[10:12], uint16(len(data)))
	return append(rr, data...)
}

func readName(msg []byte, off int) (string, int, error) {
	var labels []string
	for {
		if off >= len(msg) {
			return "", 0, errors.New("name runs past the message")
		}
		n := int(msg[off])
		off++
		if n == 0 {
			return strings.Join(labels, "."), off, nil
		}
		if n&0xC0 != 0 || off+n > len(msg) {
			return "", 0, errors.New("bad label")
		}
		labels = append(labels, string(msg[off:off+n]))
		off += n
	}
}

func (d *dnsServer) serveUDP(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		resp := d.answer(buf[:n])
		if resp == nil {
			continue
		}
		if len(resp) > maxUDPMessage {
			resp = resp[:12]
			binary.BigEndian.PutUint16(resp[2:4], binary.BigEndian.Uint16(resp[2:4])|0x0200)
			binary.BigEndian.PutUint16(resp[4:6], 0)
			binary.BigEndian.PutUint16(resp[6:8], 0)
		}
		if _, err := pc.WriteTo(resp, from); err != nil {
			log.Printf("dns: reply to %s: %v", from, err)
		}
	}
}

func (d *dnsServer) serveTCP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go d.handleTCP(conn)
	}
}

func (d *dnsServer) handleTCP(conn net.Conn) {
	defer conn.Close()
	for {
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, msg); err != nil {
			return
		}
		resp := d.answer(msg)
		if resp == nil {
			return
		}
		out := make([]byte, 2, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		if _, err := conn.Write(append(out, resp...)); err != nil {
			return
		}
	}
}
