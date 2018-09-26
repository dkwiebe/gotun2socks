package tun2socks

import (
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dkwiebe/gotun2socks/internal/gosocks"
	"github.com/dkwiebe/gotun2socks/internal/packet"
)

type tcpPacket struct {
	ip     *packet.IPv4
	tcp    *packet.TCP
	mtuBuf []byte
	wire   []byte
}

type tcpState byte

const (
	// simplified server-side tcp states
	CLOSED      tcpState = 0x0
	SYN_RCVD    tcpState = 0x1
	ESTABLISHED tcpState = 0x2
	FIN_WAIT_1  tcpState = 0x3
	FIN_WAIT_2  tcpState = 0x4
	CLOSING     tcpState = 0x5
	LAST_ACK    tcpState = 0x6
	TIME_WAIT   tcpState = 0x7

	MAX_RECV_WINDOW int = 65535
	MAX_SEND_WINDOW int = 65535

	CONNECT_NOT_SENT    = -1
	CONNECT_SENT        = 0
	CONNECT_ESTABLISHED = 1
)

type tcpConnTrack struct {
	t2s *Tun2Socks
	id  string

	input        chan *tcpPacket
	toTunCh      chan<- interface{}
	fromSocksCh  chan []byte
	toSocksCh    chan *tcpPacket
	socksCloseCh chan bool
	quitBySelf   chan bool
	quitByOther  chan bool

	connectState int

	lastPacketTime time.Time

	socksConn *gosocks.SocksConn

	// tcp context
	state tcpState
	// sequence I should use to send next segment
	// also as ack I expect in next received segment
	nxtSeq uint32
	// sequence I want in next received segment
	rcvNxtSeq uint32
	// what I have acked
	lastAck uint32

	// flow control
	recvWindow  int32
	sendWindow  int32
	sendWndCond *sync.Cond
	recvWndCond *sync.Cond

	localIP    net.IP
	remoteIP   net.IP
	localPort  uint16
	remotePort uint16
	uid        int

	proxyServer *ProxyServer
}

var (
	tcpPacketPool *sync.Pool = &sync.Pool{
		New: func() interface{} {
			return &tcpPacket{}
		},
	}
)

func tcpflagsString(tcp *packet.TCP) string {
	s := []string{}
	if tcp.SYN {
		s = append(s, "SYN")
	}
	if tcp.RST {
		s = append(s, "RST")
	}
	if tcp.FIN {
		s = append(s, "FIN")
	}
	if tcp.ACK {
		s = append(s, "ACK")
	}
	if tcp.PSH {
		s = append(s, "PSH")
	}
	if tcp.URG {
		s = append(s, "URG")
	}
	if tcp.ECE {
		s = append(s, "ECE")
	}
	if tcp.CWR {
		s = append(s, "CWR")
	}
	return strings.Join(s, ",")
}

func tcpstateString(state tcpState) string {
	switch state {
	case CLOSED:
		return "CLOSED"
	case SYN_RCVD:
		return "SYN_RCVD"
	case ESTABLISHED:
		return "ESTABLISHED"
	case FIN_WAIT_1:
		return "FIN_WAIT_1"
	case FIN_WAIT_2:
		return "FIN_WAIT_2"
	case CLOSING:
		return "CLOSING"
	case LAST_ACK:
		return "LAST_ACK"
	case TIME_WAIT:
		return "TIME_WAIT"
	}
	return ""
}

func newTCPPacket() *tcpPacket {
	return tcpPacketPool.Get().(*tcpPacket)
}

func releaseTCPPacket(pkt *tcpPacket) {
	packet.ReleaseIPv4(pkt.ip)
	packet.ReleaseTCP(pkt.tcp)
	if pkt.mtuBuf != nil {
		releaseBuffer(pkt.mtuBuf)
	}
	pkt.mtuBuf = nil
	pkt.wire = nil
	tcpPacketPool.Put(pkt)
}

func copyTCPPacket(raw []byte, ip *packet.IPv4, tcp *packet.TCP) *tcpPacket {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()
	pkt := newTCPPacket()

	// make a deep copy
	var buf []byte
	if len(raw) <= MTU {
		buf = newBuffer()
		pkt.mtuBuf = buf
	} else {
		buf = make([]byte, len(raw))
	}
	n := copy(buf, raw)
	pkt.wire = buf[:n]
	packet.ParseIPv4(pkt.wire, iphdr)
	packet.ParseTCP(iphdr.Payload, tcphdr)
	pkt.ip = iphdr
	pkt.tcp = tcphdr

	return pkt
}

func tcpConnID(ip *packet.IPv4, tcp *packet.TCP) string {
	uid := FindAppUid(ip.SrcIP.String(), tcp.SrcPort, ip.DstIP.String(), tcp.DstPort)
	return strings.Join([]string{
		fmt.Sprintf("%d", uid),
		ip.DstIP.String(),
		fmt.Sprintf("%d", tcp.DstPort),
	}, "|")
}

func packTCP(ip *packet.IPv4, tcp *packet.TCP) *tcpPacket {
	pkt := newTCPPacket()
	pkt.ip = ip
	pkt.tcp = tcp

	buf := newBuffer()
	pkt.mtuBuf = buf

	payloadL := len(tcp.Payload)
	payloadStart := MTU - payloadL
	if payloadL != 0 {
		copy(pkt.mtuBuf[payloadStart:], tcp.Payload)
	}
	tcpHL := tcp.HeaderLength()
	tcpStart := payloadStart - tcpHL
	pseduoStart := tcpStart - packet.IPv4_PSEUDO_LENGTH
	ip.PseudoHeader(pkt.mtuBuf[pseduoStart:tcpStart], packet.IPProtocolTCP, tcpHL+payloadL)
	tcp.Serialize(pkt.mtuBuf[tcpStart:payloadStart], pkt.mtuBuf[pseduoStart:])
	ipHL := ip.HeaderLength()
	ipStart := tcpStart - ipHL
	ip.Serialize(pkt.mtuBuf[ipStart:tcpStart], tcpHL+payloadL)
	pkt.wire = pkt.mtuBuf[ipStart:]
	return pkt
}

func rst(srcIP net.IP, dstIP net.IP, srcPort uint16, dstPort uint16, seq uint32, ack uint32, payloadLen uint32) *tcpPacket {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()

	iphdr.Version = 4
	iphdr.Id = packet.IPID()
	iphdr.DstIP = srcIP
	iphdr.SrcIP = dstIP
	iphdr.TTL = 64
	iphdr.Protocol = packet.IPProtocolTCP

	tcphdr.DstPort = srcPort
	tcphdr.SrcPort = dstPort
	tcphdr.Window = uint16(MAX_RECV_WINDOW)
	tcphdr.RST = true
	tcphdr.ACK = true
	tcphdr.Seq = 0

	// RFC 793:
	// "If the incoming segment has an ACK field, the reset takes its sequence
	// number from the ACK field of the segment, otherwise the reset has
	// sequence number zero and the ACK field is set to the sum of the sequence
	// number and segment length of the incoming segment. The connection remains
	// in the CLOSED state."
	tcphdr.Ack = seq + payloadLen
	if tcphdr.Ack == seq {
		tcphdr.Ack += 1
	}
	if ack != 0 {
		tcphdr.Seq = ack
	}
	return packTCP(iphdr, tcphdr)
}

func rstByPacket(pkt *tcpPacket) *tcpPacket {
	return rst(pkt.ip.SrcIP, pkt.ip.DstIP, pkt.tcp.SrcPort, pkt.tcp.DstPort, pkt.tcp.Seq, pkt.tcp.Ack, uint32(len(pkt.tcp.Payload)))
}

func (tt *tcpConnTrack) changeState(nxt tcpState) {
	// log.Printf("### [%s -> %s]", tcpstateString(tt.state), tcpstateString(nxt))
	tt.state = nxt
}

func (tt *tcpConnTrack) validAck(pkt *tcpPacket) bool {
	ret := (pkt.tcp.Ack == tt.nxtSeq)
	if !ret {
		// log.Printf("WARNING: invalid ack: recvd: %d, expecting: %d", pkt.tcp.Ack, tt.nxtSeq)
	}
	return ret
}

func (tt *tcpConnTrack) validSeq(pkt *tcpPacket) bool {
	ret := (pkt.tcp.Seq == tt.rcvNxtSeq)
	if !ret {
		// log.Printf("WARNING: invalid seq: recvd: %d, expecting: %d", pkt.tcp.Seq, tt.rcvNxtSeq)

	}
	return ret
}

func (tt *tcpConnTrack) relayPayload(pkt *tcpPacket) bool {
	log.Print("relayPayload")
	payloadLen := uint32(len(pkt.tcp.Payload))
	select {
	case tt.toSocksCh <- pkt:
		tt.rcvNxtSeq += payloadLen

		// reduce window when recved
		wnd := atomic.LoadInt32(&tt.recvWindow)
		wnd -= int32(payloadLen)
		if wnd < 0 {
			wnd = 0
		}
		atomic.StoreInt32(&tt.recvWindow, wnd)

		return true
	case <-tt.socksCloseCh:
		return false
	}
}

func (tt *tcpConnTrack) send(pkt *tcpPacket) {
	// log.Printf("<-- [TCP][%s][%s][seq:%d][ack:%d][payload:%d]", tt.id, tcpflagsString(pkt.tcp), pkt.tcp.Seq, pkt.tcp.Ack, len(pkt.tcp.Payload))
	if pkt.tcp.ACK {
		tt.lastAck = pkt.tcp.Ack
	}
	tt.toTunCh <- pkt
}

func (tt *tcpConnTrack) synAck(syn *tcpPacket) {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()

	iphdr.Version = 4
	iphdr.Id = packet.IPID()
	iphdr.SrcIP = tt.remoteIP
	iphdr.DstIP = tt.localIP
	iphdr.TTL = 64
	iphdr.Protocol = packet.IPProtocolTCP

	tcphdr.SrcPort = tt.remotePort
	tcphdr.DstPort = tt.localPort
	tcphdr.Window = uint16(atomic.LoadInt32(&tt.recvWindow))
	tcphdr.SYN = true
	tcphdr.ACK = true
	tcphdr.Seq = tt.nxtSeq
	tcphdr.Ack = tt.rcvNxtSeq

	tcphdr.Options = []packet.TCPOption{{2, 4, []byte{0x5, 0xb4}}}

	synAck := packTCP(iphdr, tcphdr)
	tt.send(synAck)
	// SYN counts 1 seq
	tt.nxtSeq += 1
}

func (tt *tcpConnTrack) finAck() {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()

	iphdr.Version = 4
	iphdr.Id = packet.IPID()
	iphdr.SrcIP = tt.remoteIP
	iphdr.DstIP = tt.localIP
	iphdr.TTL = 64
	iphdr.Protocol = packet.IPProtocolTCP

	tcphdr.SrcPort = tt.remotePort
	tcphdr.DstPort = tt.localPort
	tcphdr.Window = uint16(atomic.LoadInt32(&tt.recvWindow))
	tcphdr.FIN = true
	tcphdr.ACK = true
	tcphdr.Seq = tt.nxtSeq
	tcphdr.Ack = tt.rcvNxtSeq

	finAck := packTCP(iphdr, tcphdr)
	tt.send(finAck)
	// FIN counts 1 seq
	tt.nxtSeq += 1
}

func (tt *tcpConnTrack) ack() {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()

	iphdr.Version = 4
	iphdr.Id = packet.IPID()
	iphdr.SrcIP = tt.remoteIP
	iphdr.DstIP = tt.localIP
	iphdr.TTL = 64
	iphdr.Protocol = packet.IPProtocolTCP

	tcphdr.SrcPort = tt.remotePort
	tcphdr.DstPort = tt.localPort
	tcphdr.Window = uint16(atomic.LoadInt32(&tt.recvWindow))
	tcphdr.ACK = true
	tcphdr.Seq = tt.nxtSeq
	tcphdr.Ack = tt.rcvNxtSeq

	ack := packTCP(iphdr, tcphdr)
	tt.send(ack)
}

func (tt *tcpConnTrack) payload(data []byte) {
	iphdr := packet.NewIPv4()
	tcphdr := packet.NewTCP()

	iphdr.Version = 4
	iphdr.Id = packet.IPID()
	iphdr.SrcIP = tt.remoteIP
	iphdr.DstIP = tt.localIP
	iphdr.TTL = 64
	iphdr.Protocol = packet.IPProtocolTCP

	tcphdr.SrcPort = tt.remotePort
	tcphdr.DstPort = tt.localPort
	tcphdr.Window = uint16(atomic.LoadInt32(&tt.recvWindow))
	tcphdr.ACK = true
	tcphdr.PSH = true
	tcphdr.Seq = tt.nxtSeq
	tcphdr.Ack = tt.rcvNxtSeq
	tcphdr.Payload = data

	pkt := packTCP(iphdr, tcphdr)
	tt.send(pkt)
	// adjust seq
	tt.nxtSeq = tt.nxtSeq + uint32(len(data))
}

// stateClosed receives a SYN packet, tries to connect the socks proxy, gives a
// SYN/ACK if success, otherwise RST
func (tt *tcpConnTrack) stateClosed(syn *tcpPacket) (continu bool, release bool) {
	var e error
	if !isPrivate(tt.remoteIP) && (tt.remotePort == 80 || tt.remotePort == 443) {
		if tt.uid == -1 {
			log.Printf("initiating connection, loading uid and proxy")
			uid := FindAppUid(tt.localIP.String(), tt.localPort, tt.remoteIP.String(), tt.remotePort)
			tt.uid = uid
			tt.loadProxyConfig()
		}

		if tt.proxyServer.ProxyType == PROXY_TYPE_SOCKS {
			tt.socksConn, e = dialLocalSocks(tt.proxyServer) //only 80 and 443 goes to proxy
		} else if tt.proxyServer.ProxyType == PROXY_TYPE_HTTP {
			tt.socksConn, e = dialTransaprent(tt.proxyServer.IpAddress)
			if len(syn.tcp.Hostname) > 0 && tt.proxyServer.ProxyType == PROXY_TYPE_HTTP && tt.remotePort == 443 {
				log.Print("Connect using state closed")
				tt.callHttpProxyConnect(tt.socksConn, tt.remoteIP, syn.tcp)
			}
		} else {
			remoteIpPort := fmt.Sprintf("%s:%d", tt.remoteIP.String(), tt.remotePort)
			tt.socksConn, e = dialTransaprent(remoteIpPort)
		}
	} else {
		remoteIpPort := fmt.Sprintf("%s:%d", tt.remoteIP.String(), tt.remotePort)
		tt.socksConn, e = dialTransaprent(remoteIpPort)
	}

	if e != nil {
		log.Printf("fail to connect SOCKS proxy: %s", e)
		return
	} else {
		// no timeout
		tt.socksConn.SetDeadline(time.Time{})
	}

	if tt.socksConn == nil || tt.connectState != CONNECT_NOT_SENT {
		resp := rstByPacket(syn)
		tt.toTunCh <- resp.wire
		// log.Printf("<-- [TCP][%s][RST]", tt.id)
		return false, true
	}

	// context variables
	tt.rcvNxtSeq = syn.tcp.Seq + 1
	tt.nxtSeq = 1

	tt.synAck(syn)
	tt.changeState(SYN_RCVD)
	return true, true
}

func (tt *tcpConnTrack) callSocks(dstIP net.IP, dstPort uint16, conn net.Conn, closeCh chan bool) error {
	log.Printf("callSocks")
	_, e := gosocks.WriteSocksRequest(conn, &gosocks.SocksRequest{
		Cmd:      gosocks.SocksCmdConnect,
		HostType: gosocks.SocksIPv4Host,
		DstHost:  dstIP.String(),
		DstPort:  dstPort,
	})
	if e != nil {
		log.Printf("error to send socks request: %s", e)
		conn.Close()
		close(closeCh)
		return e
	}
	reply, e := gosocks.ReadSocksReply(conn)
	if e != nil {
		log.Printf("error to read socks reply: %s", e)
		conn.Close()
		close(closeCh)
		return e
	}
	if reply.Rep != gosocks.SocksSucceeded {
		log.Printf("socks connect request fail, retcode: %d", reply.Rep)
		conn.Close()
		close(closeCh)
		return e
	}

	return nil
}

func (tt *tcpConnTrack) callHttpProxyConnect(conn net.Conn, dstIp net.IP, tcp *packet.TCP) error {
	//log.Printf("callHttpProxyConnect")
	//"CONNECT %s:443 HTTP/1.1\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n",
	if len(tcp.Hostname) == 0 {
		tcp.Hostname = dstIp.String()
	}
	connectString := fmt.Sprintf("CONNECT %s:443 HTTP/1.1\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n", tcp.Hostname, tt.proxyServer.AuthHeader)
	//	log.Printf("%s", connectString)
	_, err := conn.Write([]byte(connectString))
	if err != nil {
		log.Println(err)
		return fmt.Errorf("Can't connect to proxy")
	}

	return nil
}

func (tt *tcpConnTrack) loadProxyConfig() {
	log.Printf("loadProxyConfig for uid %d", tt.uid)

	proxyServer, ok := tt.t2s.proxyServerMap[tt.uid]
	if !ok {
		tt.proxyServer = tt.t2s.defaultProxyServer
	} else {
		tt.proxyServer = proxyServer
	}

	log.Printf("Proxy selected: address %s, type: %d", tt.proxyServer.IpAddress, tt.proxyServer.ProxyType)
}

func (tt *tcpConnTrack) tcpSocks2Tun(dstIP net.IP, dstPort uint16, conn net.Conn, readCh chan<- []byte, writeCh <-chan *tcpPacket, closeCh chan bool) {
	log.Printf("tcpSocks2Tun %s", dstIP.String())
	if tt.uid == -1 {
		uid := FindAppUid(tt.localIP.String(), tt.localPort, dstIP.String(), dstPort)
		log.Printf("UID for TCP request from %s:%d to %s:%d is %d", tt.localIP.String(), tt.localPort, dstIP.String(), dstPort, uid)
		tt.uid = uid
		tt.loadProxyConfig()
	}

	if !isPrivate(dstIP) && (dstPort == 443 || dstPort == 80) {
		if tt.proxyServer.ProxyType == PROXY_TYPE_SOCKS {
			e := tt.callSocks(dstIP, dstPort, conn, closeCh)
			if e != nil {
				return
			}
		}
	}
	if tt.proxyServer.ProxyType != PROXY_TYPE_HTTP || dstPort != 443 || isPrivate(dstIP) {
		tt.connectState = CONNECT_ESTABLISHED
	}

	// writer
	go func() {
	loop:
		for {
			select {
			case <-closeCh:
				break loop
			case pkt := <-writeCh:
				if tt.connectState == CONNECT_NOT_SENT {
					err := tt.callHttpProxyConnect(conn, dstIP, pkt.tcp)
					if err != nil {
						log.Printf("Can't send connect request")
					}

					tt.connectState = CONNECT_SENT
				}

				if tt.proxyServer.ProxyType == PROXY_TYPE_HTTP {
					if tt.connectState != CONNECT_ESTABLISHED {
						tt.recvWndCond.L.Lock()
						log.Print("Waiting https connect")
						tt.recvWndCond.Wait()
						tt.recvWndCond.L.Unlock()
					}

					if pkt.tcp.DstPort == 443 {
						log.Printf("Sending https data")
						conn.Write(pkt.tcp.Payload)
					} else {
						log.Printf("Sending http data %s", pkt.tcp.Hostname)
						conn.Write(pkt.tcp.PatchHostForPlainHttp(tt.proxyServer.AuthHeader))
					}

				} else {
					conn.Write(pkt.tcp.Payload)
				}

				// increase window when processed
				wnd := atomic.LoadInt32(&tt.recvWindow)
				wnd += int32(len(pkt.tcp.Payload))
				if wnd > int32(MAX_RECV_WINDOW) {
					wnd = int32(MAX_RECV_WINDOW)
				}
				atomic.StoreInt32(&tt.recvWindow, wnd)

				releaseTCPPacket(pkt)
			}
		}
		log.Print("Writer exit routine")
	}()

	// reader
	for {
		var buf [MTU - 40]byte

		// tt.sendWndCond.L.Lock()
		var wnd int32
		var cur int32
		wnd = atomic.LoadInt32(&tt.sendWindow)

		if wnd <= 0 {
			for wnd <= 0 {
				tt.sendWndCond.L.Lock()
				tt.sendWndCond.Wait()
				wnd = atomic.LoadInt32(&tt.sendWindow)
			}
			tt.sendWndCond.L.Unlock()
		}

		cur = wnd
		if cur > MTU-40 {
			cur = MTU - 40
		}
		// tt.sendWndCond.L.Unlock()
		if tt.connectState == CONNECT_SENT {
			conn.Read(buf[:])
			log.Print("Reading connect")
			tt.connectState = CONNECT_ESTABLISHED
			tt.recvWndCond.Broadcast()
		} else if tt.connectState == CONNECT_ESTABLISHED {
			n, e := conn.Read(buf[:cur])
			log.Print("Reading data")
			if e != nil {
				log.Printf("error to read from socks: %s", e)
				conn.Close()
				break
			} else {
				b := make([]byte, n)
				copy(b, buf[:n])
				readCh <- b

				// tt.sendWndCond.L.Lock()
				nxt := wnd - int32(n)
				if nxt < 0 {
					nxt = 0
				}
				// if sendWindow does not equal to wnd, it is already updated by a
				// received pkt from TUN
				atomic.CompareAndSwapInt32(&tt.sendWindow, wnd, nxt)
				// tt.sendWndCond.L.Unlock()
			}
		}
		tt.recvWndCond.Broadcast()
	}

	tt.recvWndCond.Broadcast()
	closeCh <- true
	log.Print("Reader exit routine")
	close(closeCh)
}

// stateSynRcvd expects a ACK with matching ack number,
func (tt *tcpConnTrack) stateSynRcvd(pkt *tcpPacket) (continu bool, release bool) {
	//log.Print("stateSynRcvd")
	// rst to packet with invalid sequence/ack, state unchanged
	if !(tt.validSeq(pkt) && tt.validAck(pkt)) {
		if !pkt.tcp.RST {
			resp := rstByPacket(pkt)
			tt.toTunCh <- resp
			// log.Printf("<-- [TCP][%s][RST] continue", tt.id)
		}
		return true, true
	}
	// connection ends by valid RST
	if pkt.tcp.RST {
		return false, true
	}
	// ignore non-ACK packets
	if !pkt.tcp.ACK {
		return true, true
	}
	continu = true
	release = true
	tt.changeState(ESTABLISHED)
	go tt.tcpSocks2Tun(tt.remoteIP, uint16(tt.remotePort), tt.socksConn, tt.fromSocksCh, tt.toSocksCh, tt.socksCloseCh)
	if len(pkt.tcp.Payload) != 0 {
		if tt.relayPayload(pkt) {
			// pkt hands to socks writer
			release = false
		}
	}
	return
}

func (tt *tcpConnTrack) stateEstablished(pkt *tcpPacket) (continu bool, release bool) {
	// ack if sequence is not expected
	if !tt.validSeq(pkt) {
		tt.ack()

		return true, true
	}
	// connection ends by valid RST
	if pkt.tcp.RST {
		log.Print("rst")
		return false, true
	}
	// ignore non-ACK packets
	if !pkt.tcp.ACK {
		log.Print("ack")
		return true, true
	}

	continu = true
	release = true
	if len(pkt.tcp.Payload) != 0 {
		if tt.relayPayload(pkt) {
			// pkt hands to socks writer
			release = false
		}
	}
	if pkt.tcp.FIN {
		tt.rcvNxtSeq += 1
		tt.finAck()
		tt.changeState(LAST_ACK)
		tt.socksConn.Close()
	}
	return
}

func (tt *tcpConnTrack) stateFinWait1(pkt *tcpPacket) (continu bool, release bool) {
	//log.Print("stateFinWait1")
	// ignore packet with invalid sequence, state unchanged
	if !tt.validSeq(pkt) {
		return false, true
	}
	// connection ends by valid RST
	if pkt.tcp.RST {
		return false, true
	}
	// ignore non-ACK packets
	if !pkt.tcp.ACK {
		return false, true
	}

	if pkt.tcp.FIN {
		tt.rcvNxtSeq += 1
		tt.ack()
		if pkt.tcp.ACK && tt.validAck(pkt) {
			tt.changeState(TIME_WAIT)
			return false, true
		} else {
			tt.changeState(CLOSING)
			return false, true
		}
	} else {
		tt.changeState(FIN_WAIT_2)
		return false, true
	}
}

func (tt *tcpConnTrack) stateFinWait2(pkt *tcpPacket) (continu bool, release bool) {
	//log.Print("stateFinWait2")
	// ignore packet with invalid sequence/ack, state unchanged
	if !(tt.validSeq(pkt) && tt.validAck(pkt)) {
		return false, true
	}
	// connection ends by valid RST
	if pkt.tcp.RST {
		return false, true
	}
	// ignore non-FIN non-ACK packets
	if !pkt.tcp.ACK || !pkt.tcp.FIN {
		return false, true
	}
	tt.rcvNxtSeq += 1
	tt.ack()
	tt.changeState(TIME_WAIT)
	return false, true
}

func (tt *tcpConnTrack) stateClosing(pkt *tcpPacket) (continu bool, release bool) {
	//log.Print("stateClosing")
	// ignore packet with invalid sequence/ack, state unchanged
	if !(tt.validSeq(pkt) && tt.validAck(pkt)) {
		return true, true
	}
	// connection ends by valid RST
	if pkt.tcp.RST {
		return false, true
	}
	// ignore non-ACK packets
	if !pkt.tcp.ACK {
		return true, true
	}
	tt.changeState(TIME_WAIT)
	return false, true
}

func (tt *tcpConnTrack) stateLastAck(pkt *tcpPacket) (continu bool, release bool) {
	//log.Print("stateLastAck")
	// ignore packet with invalid sequence/ack, state unchanged
	if !(tt.validSeq(pkt) && tt.validAck(pkt)) {
		return true, true
	}
	// ignore non-ACK packets
	if !pkt.tcp.ACK {
		return true, true
	}
	// connection ends
	tt.changeState(CLOSED)
	return false, true
}

func (tt *tcpConnTrack) newPacket(pkt *tcpPacket) {
	//log.Print("newPacket")
	select {
	case <-tt.quitByOther:
	case <-tt.quitBySelf:
	case tt.input <- pkt:
	}
}

func (tt *tcpConnTrack) updateSendWindow(pkt *tcpPacket) {
	//log.Print("updateSendWindow")
	// tt.sendWndCond.L.Lock()
	atomic.StoreInt32(&tt.sendWindow, int32(pkt.tcp.Window))
	tt.sendWndCond.Signal()
	// tt.sendWndCond.L.Unlock()
}

func (tt *tcpConnTrack) run() bool {
	var fromSocksCh chan []byte
	// enable some channels only when the state is ESTABLISHED
	if tt.state == ESTABLISHED {
		fromSocksCh = tt.fromSocksCh
	}

	select {
	case pkt := <-tt.input:
		tt.lastPacketTime = time.Now()

		//log.Printf("--> [TCP][%s][%s][%s][seq:%d][ack:%d][payload:%d]", tt.id, tcpstateString(tt.state), tcpflagsString(pkt.tcp), pkt.tcp.Seq, pkt.tcp.Ack, len(pkt.tcp.Payload))
		var continu, release bool

		tt.updateSendWindow(pkt)

		switch tt.state {
		case CLOSED:
			continu, release = tt.stateClosed(pkt)
		case SYN_RCVD:
			continu, release = tt.stateSynRcvd(pkt)
		case ESTABLISHED:
			continu, release = tt.stateEstablished(pkt)
		case FIN_WAIT_1:
			continu, release = tt.stateFinWait1(pkt)
		case FIN_WAIT_2:
			continu, release = tt.stateFinWait2(pkt)
		case CLOSING:
			continu, release = tt.stateClosing(pkt)
		case LAST_ACK:
			continu, release = tt.stateLastAck(pkt)
		}

		if tt.state == FIN_WAIT_1 || tt.state == FIN_WAIT_2 {
			tt.finAck()
		}

		if !continu {
			if tt.socksConn != nil {
				tt.socksConn.Close()
			}
			close(tt.quitBySelf)
			tt.t2s.clearTCPConnTrack(tt.id)
			return true
		}

		if release {
			releaseTCPPacket(pkt)
		}
	case data := <-fromSocksCh:
		tt.lastPacketTime = time.Now()
		tt.payload(data)
	default:
		return false
	}
	return true
}

func (t2s *Tun2Socks) createTCPConnTrack(id string, ip *packet.IPv4, tcp *packet.TCP) *tcpConnTrack {
	t2s.tcpConnTrackLock.Lock()
	defer t2s.tcpConnTrackLock.Unlock()

	track := &tcpConnTrack{
		t2s:          t2s,
		id:           id,
		toTunCh:      t2s.writeCh,
		input:        make(chan *tcpPacket, 10),
		fromSocksCh:  make(chan []byte, 20),
		toSocksCh:    make(chan *tcpPacket, 20),
		socksCloseCh: make(chan bool, 100),
		quitBySelf:   make(chan bool),
		quitByOther:  make(chan bool),
		connectState: CONNECT_NOT_SENT,

		lastPacketTime: time.Now(),

		sendWindow:  int32(MAX_SEND_WINDOW),
		recvWindow:  int32(MAX_RECV_WINDOW),
		sendWndCond: &sync.Cond{L: &sync.Mutex{}},
		recvWndCond: &sync.Cond{L: &sync.Mutex{}},

		localPort:  tcp.SrcPort,
		remotePort: tcp.DstPort,
		state:      CLOSED,

		uid:         FindAppUid(ip.SrcIP.String(), tcp.SrcPort, ip.DstIP.String(), tcp.DstPort),
		proxyServer: t2s.defaultProxyServer,
	}

	track.localIP = make(net.IP, len(ip.SrcIP))
	copy(track.localIP, ip.SrcIP)
	track.remoteIP = make(net.IP, len(ip.DstIP))
	copy(track.remoteIP, ip.DstIP)

	track.loadProxyConfig()

	t2s.tcpConnTrackMap[id] = track
	return track
}

func (t2s *Tun2Socks) getTCPConnTrack(id string) *tcpConnTrack {
	return t2s.tcpConnTrackMap[id]
}

func (t2s *Tun2Socks) clearTCPConnTrack(id string) {
	track, ok := t2s.tcpConnTrackMap[id]
	if ok {
		track.recvWndCond.Broadcast()
		track.sendWndCond.Broadcast()
	}

	delete(t2s.tcpConnTrackMap, id)
}

func (t2s *Tun2Socks) tcp(raw []byte, ip *packet.IPv4, tcp *packet.TCP) {
	connID := tcpConnID(ip, tcp)

	track := t2s.getTCPConnTrack(connID)

	if track != nil {
		pkt := copyTCPPacket(raw, ip, tcp)
		track.newPacket(pkt)
	} else {
		// ignore RST, if there is no track of this connection
		if tcp.RST {
			// log.Printf("--> [TCP][%s][%s]", connID, tcpflagsString(tcp))
			return
		}
		// return a RST to non-SYN packet
		if !tcp.SYN {
			// log.Printf("--> [TCP][%s][%s]", connID, tcpflagsString(tcp))
			resp := rst(ip.SrcIP, ip.DstIP, tcp.SrcPort, tcp.DstPort, tcp.Seq, tcp.Ack, uint32(len(tcp.Payload)))
			t2s.writeCh <- resp
			// log.Printf("<-- [TCP][%s][RST]", connID)
			return
		}

		pkt := copyTCPPacket(raw, ip, tcp)
		track := t2s.createTCPConnTrack(connID, ip, tcp)
		track.newPacket(pkt)
	}
}
