package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pin/tftp/v3"
)

// PXE boot of the rescue systems:
//   - proxyDHCP (UDP 67/4011) tells PXE clients where to boot from, without
//     touching the existing DHCP server
//   - TFTP serves iPXE (data/pxe/ipxe.efi, data/pxe/undionly.kpxe)
//   - plain HTTP on :8080 serves the iPXE menu, wimboot, WinPE and the
//     Linux rescue kernel (iPXE can not use the pinned HTTPS certificate)

func (s *Server) pxeIP() net.IP {
	if v := s.setting("pxe_ip", ""); v != "" {
		return net.ParseIP(v).To4()
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.To4()
		}
	}
	return nil
}

func (s *Server) runPXE(ctx context.Context) {
	ip := s.pxeIP()
	if ip == nil {
		s.log.Printf("PXE: keine IPv4-Adresse gefunden (Einstellung pxe_ip setzen)")
		return
	}
	s.log.Printf("PXE aktiv auf %s (TFTP 69, HTTP 8080, proxyDHCP %v)", ip, s.setting("pxe_dhcp", "1") == "1")

	ts := tftp.NewServer(func(name string, rf io.ReaderFrom) error {
		f, err := os.Open(s.path("pxe", filepath.Base(name)))
		if err != nil {
			return err
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil {
			if t, ok := rf.(tftp.OutgoingTransfer); ok {
				t.SetSize(st.Size())
			}
		}
		_, err = rf.ReadFrom(f)
		return err
	}, nil)
	ts.SetTimeout(5 * time.Second)
	go func() {
		if err := ts.ListenAndServe(":69"); err != nil {
			s.log.Printf("TFTP: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/pxe/boot.ipxe", func(w http.ResponseWriter, r *http.Request) {
		base := fmt.Sprintf("http://%s:8080/pxe", ip)
		fmt.Fprintf(w, `#!ipxe
dhcp
menu Bastion Wiederherstellung
item --gap Server: %[2]s
item winpe  Windows wiederherstellen (WinPE)
item linux  Linux wiederherstellen
item local  Von Festplatte starten
choose --default local --timeout 30000 target && goto ${target}
:winpe
kernel %[1]s/wimboot
initrd -n boot.wim %[1]s/winpe.wim boot.wim
boot
:linux
kernel %[1]s/linux-vmlinuz bastion.server=%[2]s bastion.fp=%[3]s quiet
initrd %[1]s/linux-initrd.img
boot
:local
exit
`, base, s.PublicURL(), s.fp)
	})
	mux.HandleFunc("/pxe/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		switch name {
		case "winpe.wim", "linux-vmlinuz", "linux-initrd.img":
			http.ServeFile(w, r, s.path("rescue", name))
		case "wimboot", "ipxe.efi", "undionly.kpxe":
			http.ServeFile(w, r, s.path("pxe", name))
		default:
			http.NotFound(w, r)
		}
	})
	hs := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Printf("PXE-HTTP: %v", err)
		}
	}()

	if s.setting("pxe_dhcp", "1") == "1" {
		go s.proxyDHCP(ctx, ip, 67)
		go s.proxyDHCP(ctx, ip, 4011)
	}
	<-ctx.Done()
	ts.Shutdown()
	hs.Close()
}

// proxyDHCP answers PXE clients with boot server and file name only.
func (s *Server) proxyDHCP(ctx context.Context, ip net.IP, port int) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		s.log.Printf("proxyDHCP :%d: %v", port, err)
		return
	}
	go func() { <-ctx.Done(); conn.Close() }()
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		reply := pxeReply(buf[:n], ip, port)
		if reply == nil {
			continue
		}
		dst := from
		if port == 67 {
			dst = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
		}
		conn.WriteToUDP(reply, dst)
	}
}

func dhcpOptions(b []byte) map[byte][]byte {
	opts := map[byte][]byte{}
	for i := 0; i < len(b); {
		code := b[i]
		if code == 0 {
			i++
			continue
		}
		if code == 255 || i+1 >= len(b) {
			break
		}
		l := int(b[i+1])
		if i+2+l > len(b) {
			break
		}
		opts[code] = b[i+2 : i+2+l]
		i += 2 + l
	}
	return opts
}

func pxeReply(req []byte, ip net.IP, port int) []byte {
	if len(req) < 240 || req[0] != 1 || binary.BigEndian.Uint32(req[236:240]) != 0x63825363 {
		return nil
	}
	opts := dhcpOptions(req[240:])
	if !strings.HasPrefix(string(opts[60]), "PXEClient") {
		return nil
	}
	msgType := byte(0)
	if t := opts[53]; len(t) == 1 {
		msgType = t[0]
	}
	var replyType byte
	switch {
	case msgType == 1 && port == 67: // DISCOVER → OFFER
		replyType = 2
	case msgType == 3 && port == 4011: // REQUEST → ACK
		replyType = 5
	default:
		return nil
	}
	boot := "undionly.kpxe"
	if a := opts[93]; len(a) == 2 {
		switch binary.BigEndian.Uint16(a) {
		case 6, 7, 9, 10:
			boot = "ipxe.efi"
		}
	}
	if string(opts[77]) == "iPXE" {
		boot = fmt.Sprintf("http://%s:8080/pxe/boot.ipxe", ip)
	}
	r := make([]byte, 240, 400)
	r[0], r[1], r[2] = 2, req[1], req[2]
	copy(r[4:8], req[4:8])     // xid
	copy(r[10:12], req[10:12]) // flags
	copy(r[12:16], req[12:16]) // ciaddr
	copy(r[20:24], ip)         // siaddr: next server
	copy(r[28:44], req[28:44]) // chaddr
	copy(r[108:236], boot)     // file
	binary.BigEndian.PutUint32(r[236:240], 0x63825363)
	add := func(code byte, v []byte) { r = append(r, code, byte(len(v))); r = append(r, v...) }
	add(53, []byte{replyType})
	add(54, ip)
	add(60, []byte("PXEClient"))
	add(43, []byte{6, 1, 8, 255}) // PXE_DISCOVERY_CONTROL: boot the file directly
	if u := opts[97]; len(u) > 0 {
		add(97, u)
	}
	r = append(r, 255)
	return r
}
