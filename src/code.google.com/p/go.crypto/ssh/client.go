// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"crypto"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
)

// clientVersion is the fixed identification string that the client will use.
var clientVersion = []byte("SSH-2.0-Go\r\n")

// ClientConn represents the client side of an SSH connection.
type ClientConn struct {
	*transport
	config      *ClientConfig
	chanList    // channels associated with this connection
	forwardList // forwarded tcpip connections from the remote side
	globalRequest
}

type globalRequest struct {
	sync.Mutex
	response chan interface{}
}

// Client returns a new SSH client connection using c as the underlying transport.
func Client(c net.Conn, config *ClientConfig) (*ClientConn, error) {
	conn := &ClientConn{
		transport:     newTransport(c, config.rand()),
		config:        config,
		globalRequest: globalRequest{response: make(chan interface{}, 1)},
	}
	if err := conn.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	go conn.mainLoop()
	return conn, nil
}

// handshake performs the client side key exchange. See RFC 4253 Section 7.
func (c *ClientConn) handshake() error {
	var magics handshakeMagics

	if _, err := c.Write(clientVersion); err != nil {
		return err
	}
	if err := c.Flush(); err != nil {
		return err
	}
	magics.clientVersion = clientVersion[:len(clientVersion)-2]

	// read remote server version
	version, err := readVersion(c)
	if err != nil {
		return err
	}
	magics.serverVersion = version
	clientKexInit := kexInitMsg{
		KexAlgos:                supportedKexAlgos,
		ServerHostKeyAlgos:      supportedHostKeyAlgos,
		CiphersClientServer:     c.config.Crypto.ciphers(),
		CiphersServerClient:     c.config.Crypto.ciphers(),
		MACsClientServer:        c.config.Crypto.macs(),
		MACsServerClient:        c.config.Crypto.macs(),
		CompressionClientServer: supportedCompressions,
		CompressionServerClient: supportedCompressions,
	}
	kexInitPacket := marshal(msgKexInit, clientKexInit)
	magics.clientKexInit = kexInitPacket

	if err := c.writePacket(kexInitPacket); err != nil {
		return err
	}
	packet, err := c.readPacket()
	if err != nil {
		return err
	}

	magics.serverKexInit = packet

	var serverKexInit kexInitMsg
	if err = unmarshal(&serverKexInit, packet, msgKexInit); err != nil {
		return err
	}

	kexAlgo, hostKeyAlgo, ok := findAgreedAlgorithms(c.transport, &clientKexInit, &serverKexInit)
	if !ok {
		return errors.New("ssh: no common algorithms")
	}

	if serverKexInit.FirstKexFollows && kexAlgo != serverKexInit.KexAlgos[0] {
		// The server sent a Kex message for the wrong algorithm,
		// which we have to ignore.
		if _, err := c.readPacket(); err != nil {
			return err
		}
	}

	var H, K []byte
	var hashFunc crypto.Hash
	switch kexAlgo {
	case kexAlgoDH14SHA1:
		hashFunc = crypto.SHA1
		dhGroup14Once.Do(initDHGroup14)
		H, K, err = c.kexDH(dhGroup14, hashFunc, &magics, hostKeyAlgo)
	case keyAlgoDH1SHA1:
		hashFunc = crypto.SHA1
		dhGroup1Once.Do(initDHGroup1)
		H, K, err = c.kexDH(dhGroup1, hashFunc, &magics, hostKeyAlgo)
	default:
		err = fmt.Errorf("ssh: unexpected key exchange algorithm %v", kexAlgo)
	}
	if err != nil {
		return err
	}

	if err = c.writePacket([]byte{msgNewKeys}); err != nil {
		return err
	}
	if err = c.transport.writer.setupKeys(clientKeys, K, H, H, hashFunc); err != nil {
		return err
	}
	if packet, err = c.readPacket(); err != nil {
		return err
	}
	if packet[0] != msgNewKeys {
		return UnexpectedMessageError{msgNewKeys, packet[0]}
	}
	if err := c.transport.reader.setupKeys(serverKeys, K, H, H, hashFunc); err != nil {
		return err
	}
	return c.authenticate(H)
}

// kexDH performs Diffie-Hellman key agreement on a ClientConn. The
// returned values are given the same names as in RFC 4253, section 8.
func (c *ClientConn) kexDH(group *dhGroup, hashFunc crypto.Hash, magics *handshakeMagics, hostKeyAlgo string) ([]byte, []byte, error) {
	x, err := rand.Int(c.config.rand(), group.p)
	if err != nil {
		return nil, nil, err
	}
	X := new(big.Int).Exp(group.g, x, group.p)
	kexDHInit := kexDHInitMsg{
		X: X,
	}
	if err := c.writePacket(marshal(msgKexDHInit, kexDHInit)); err != nil {
		return nil, nil, err
	}

	packet, err := c.readPacket()
	if err != nil {
		return nil, nil, err
	}

	var kexDHReply kexDHReplyMsg
	if err = unmarshal(&kexDHReply, packet, msgKexDHReply); err != nil {
		return nil, nil, err
	}

	kInt, err := group.diffieHellman(kexDHReply.Y, x)
	if err != nil {
		return nil, nil, err
	}

	h := hashFunc.New()
	writeString(h, magics.clientVersion)
	writeString(h, magics.serverVersion)
	writeString(h, magics.clientKexInit)
	writeString(h, magics.serverKexInit)
	writeString(h, kexDHReply.HostKey)
	writeInt(h, X)
	writeInt(h, kexDHReply.Y)
	K := make([]byte, intLength(kInt))
	marshalInt(K, kInt)
	h.Write(K)

	H := h.Sum(nil)

	return H, K, nil
}

// mainLoop reads incoming messages and routes channel messages
// to their respective ClientChans.
func (c *ClientConn) mainLoop() {
	defer func() {
		c.Close()
		c.closeAll()
	}()

	for {
		packet, err := c.readPacket()
		if err != nil {
			break
		}
		// TODO(dfc) A note on blocking channel use.
		// The msg, data and dataExt channels of a clientChan can
		// cause this loop to block indefinately if the consumer does
		// not service them.
		switch packet[0] {
		case msgChannelData:
			if len(packet) < 9 {
				// malformed data packet
				return
			}
			remoteId := binary.BigEndian.Uint32(packet[1:5])
			length := binary.BigEndian.Uint32(packet[5:9])
			packet = packet[9:]

			if length != uint32(len(packet)) {
				return
			}
			ch, ok := c.getChan(remoteId)
			if !ok {
				return
			}
			ch.stdout.write(packet)
		case msgChannelExtendedData:
			if len(packet) < 13 {
				// malformed data packet
				return
			}
			remoteId := binary.BigEndian.Uint32(packet[1:5])
			datatype := binary.BigEndian.Uint32(packet[5:9])
			length := binary.BigEndian.Uint32(packet[9:13])
			packet = packet[13:]

			if length != uint32(len(packet)) {
				return
			}
			// RFC 4254 5.2 defines data_type_code 1 to be data destined
			// for stderr on interactive sessions. Other data types are
			// silently discarded.
			if datatype == 1 {
				ch, ok := c.getChan(remoteId)
				if !ok {
					return
				}
				ch.stderr.write(packet)
			}
		default:
			msg := decode(packet)
			switch msg := msg.(type) {
			case *channelOpenMsg:
				c.handleChanOpen(msg)
			case *channelOpenConfirmMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.msg <- msg
			case *channelOpenFailureMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.msg <- msg
			case *channelCloseMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.Close()
				close(ch.msg)
				c.chanList.remove(msg.PeersId)
			case *channelEOFMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.stdout.eof()
				// RFC 4254 is mute on how EOF affects dataExt messages but
				// it is logical to signal EOF at the same time.
				ch.stderr.eof()
			case *channelRequestSuccessMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.msg <- msg
			case *channelRequestFailureMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.msg <- msg
			case *channelRequestMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				ch.msg <- msg
			case *windowAdjustMsg:
				ch, ok := c.getChan(msg.PeersId)
				if !ok {
					return
				}
				if !ch.remoteWin.add(msg.AdditionalBytes) {
					// invalid window update
					return
				}
			case *globalRequestMsg:
				// This handles keepalive messages and matches
				// the behaviour of OpenSSH.
				if msg.WantReply {
					c.writePacket(marshal(msgRequestFailure, globalRequestFailureMsg{}))
				}
			case *globalRequestSuccessMsg, *globalRequestFailureMsg:
				c.globalRequest.response <- msg
			case *disconnectMsg:
				return
			default:
				fmt.Printf("mainLoop: unhandled message %T: %v\n", msg, msg)
			}
		}
	}
}

// Handle channel open messages from the remote side.
func (c *ClientConn) handleChanOpen(msg *channelOpenMsg) {
	switch msg.ChanType {
	case "forwarded-tcpip":
		laddr, rest, ok := parseTCPAddr(msg.TypeSpecificData)
		if !ok {
			// invalid request
			c.sendConnectionFailed(msg.PeersId)
			return
		}
		l, ok := c.forwardList.lookup(laddr)
		if !ok {
			fmt.Println("could not find forward list entry for", laddr)
			// Section 7.2, implementations MUST reject suprious incoming
			// connections.
			c.sendConnectionFailed(msg.PeersId)
			return
		}
		raddr, rest, ok := parseTCPAddr(rest)
		if !ok {
			// invalid request
			c.sendConnectionFailed(msg.PeersId)
			return
		}
		ch := c.newChan(c.transport)
		ch.remoteId = msg.PeersId
		ch.remoteWin.add(msg.PeersWindow)

		m := channelOpenConfirmMsg{
			PeersId:       ch.remoteId,
			MyId:          ch.localId,
			MyWindow:      1 << 14,
			MaxPacketSize: 1 << 15, // RFC 4253 6.1
		}
		c.writePacket(marshal(msgChannelOpenConfirm, m))
		l <- forward{ch, raddr}
	default:
		// unknown channel type
		m := channelOpenFailureMsg{
			PeersId:  msg.PeersId,
			Reason:   UnknownChannelType,
			Message:  fmt.Sprintf("unknown channel type: %v", msg.ChanType),
			Language: "en_US.UTF-8",
		}
		c.writePacket(marshal(msgChannelOpenFailure, m))
	}
}

// sendGlobalRequest sends a global request message as specified
// in RFC4254 section 4. To correctly synchronise messages, a lock
// is held internally until a response is returned.
func (c *ClientConn) sendGlobalRequest(m interface{}) (*globalRequestSuccessMsg, error) {
	c.globalRequest.Lock()
	defer c.globalRequest.Unlock()
	if err := c.writePacket(marshal(msgGlobalRequest, m)); err != nil {
		return nil, err
	}
	r := <-c.globalRequest.response
	if r, ok := r.(*globalRequestSuccessMsg); ok {
		return r, nil
	}
	return nil, errors.New("request failed")
}

// sendConnectionFailed rejects an incoming channel identified
// by remoteId.
func (c *ClientConn) sendConnectionFailed(remoteId uint32) error {
	m := channelOpenFailureMsg{
		PeersId:  remoteId,
		Reason:   ConnectionFailed,
		Message:  "invalid request",
		Language: "en_US.UTF-8",
	}
	return c.writePacket(marshal(msgChannelOpenFailure, m))
}

// parseTCPAddr parses the originating address from the remote into a *net.TCPAddr.
// RFC 4254 section 7.2 is mute on what to do if parsing fails but the forwardlist
// requires a valid *net.TCPAddr to operate, so we enforce that restriction here.
func parseTCPAddr(b []byte) (*net.TCPAddr, []byte, bool) {
	addr, b, ok := parseString(b)
	if !ok {
		return nil, b, false
	}
	port, b, ok := parseUint32(b)
	if !ok {
		return nil, b, false
	}
	ip := net.ParseIP(string(addr))
	if ip == nil {
		return nil, b, false
	}
	return &net.TCPAddr{IP: ip, Port: int(port)}, b, true
}

// Dial connects to the given network address using net.Dial and
// then initiates a SSH handshake, returning the resulting client connection.
func Dial(network, addr string, config *ClientConfig) (*ClientConn, error) {
	conn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	return Client(conn, config)
}

// A ClientConfig structure is used to configure a ClientConn. After one has
// been passed to an SSH function it must not be modified.
type ClientConfig struct {
	// Rand provides the source of entropy for key exchange. If Rand is
	// nil, the cryptographic random reader in package crypto/rand will
	// be used.
	Rand io.Reader

	// The username to authenticate.
	User string

	// A slice of ClientAuth methods. Only the first instance
	// of a particular RFC 4252 method will be used during authentication.
	Auth []ClientAuth

	// Cryptographic-related configuration.
	Crypto CryptoConfig
}

func (c *ClientConfig) rand() io.Reader {
	if c.Rand == nil {
		return rand.Reader
	}
	return c.Rand
}

// Thread safe channel list.
type chanList struct {
	// protects concurrent access to chans
	sync.Mutex
	// chans are indexed by the local id of the channel, clientChan.localId.
	// The PeersId value of messages received by ClientConn.mainLoop is
	// used to locate the right local clientChan in this slice.
	chans []*clientChan
}

// Allocate a new ClientChan with the next avail local id.
func (c *chanList) newChan(t *transport) *clientChan {
	c.Lock()
	defer c.Unlock()
	for i := range c.chans {
		if c.chans[i] == nil {
			ch := newClientChan(t, uint32(i))
			c.chans[i] = ch
			return ch
		}
	}
	i := len(c.chans)
	ch := newClientChan(t, uint32(i))
	c.chans = append(c.chans, ch)
	return ch
}

func (c *chanList) getChan(id uint32) (*clientChan, bool) {
	c.Lock()
	defer c.Unlock()
	if id >= uint32(len(c.chans)) {
		return nil, false
	}
	return c.chans[id], true
}

func (c *chanList) remove(id uint32) {
	c.Lock()
	defer c.Unlock()
	c.chans[id] = nil
}

func (c *chanList) closeAll() {
	c.Lock()
	defer c.Unlock()

	for _, ch := range c.chans {
		if ch == nil {
			continue
		}
		ch.Close()
		close(ch.msg)
	}
}
