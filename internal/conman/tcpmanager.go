package conman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/antihax/gambit/internal/conman/gctx"
	"github.com/antihax/gambit/internal/drivers"
	"github.com/antihax/gambit/internal/muxconn"
	"github.com/antihax/gambit/internal/store"
	"github.com/antihax/gambit/pkg/probe"
)

func (s *ConnectionManager) tcpManager() {
	conn, err := net.ListenIP("ip4:tcp", nil)
	if err != nil {
		panic(err)
	}

	go func() {
		for {
			// read max MTU if available
			buf := make([]byte, 1500)
			n, addr, err := conn.ReadFrom(buf)
			if err != nil { // get out if we error
				s.logger.Trace().Err(err).
					Str("network", "tcp").
					Str("address", addr.String()).
					Msg("reading socket")
				continue
			}

			pkt := &probe.TCPPacket{}
			pkt.Decode(buf[:n])
			if pkt.Flags&probe.SYN != 0 {
				// fire up listener, kernel will take over future requests.
				known, err := s.CreateTCPListener(pkt.DestPort)
				if err != nil {
					s.logger.Trace().Err(err).Msg("creating socket")
				}
				if !known {
					s.logger.Trace().Msgf("started tcp server: %v", pkt.DestPort)
				}
			}
		}
	}()
}

// CreateTCPListener will create a new listener if one does not already exist and return if it was created or not.
func (s *ConnectionManager) CreateTCPListener(port uint16) (bool, error) {
	var wg sync.WaitGroup

	if port > s.config.MaxPort {
		return false, errors.New("above config.Maxport")
	}

	wg.Wait()

	address := "0.0.0.0"
	if s.config.BindAddress != "" {
		if s.config.BindAddress == "public" {
			for _, addr := range s.addresses {
				if !privateIP(addr) && addr.To4() != nil {
					address = addr.String()
				}
			}
		} else {
			address = s.config.BindAddress
		}
	}

	// create a new listener if one does not already exist
	if _, ok := s.tcpListeners[port]; !ok {
		addr := fmt.Sprintf("%s:%d", address, port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return true, err
		}
		s.tcpListeners[port] = ln

		// handle the connections
		go func() {
			for {
				conn, err := ln.Accept()
				if err == nil {
					wg.Add(1)
					go s.handleConnection(conn, ln, &wg)
				}
			}
		}()

		return false, nil
	}

	return true, nil
}

func (s *ConnectionManager) handleConnection(conn net.Conn, root net.Listener, wg *sync.WaitGroup) {
	defer wg.Done()
	// ban hammers
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		if s.tickBan(addr.IP.String()) {
			conn.Close()
			return
		}
	}

	// create our sniffer
	muc := muxconn.NewMuxConn(s.RootContext, conn)
	r := muc.StartSniffing()
	port := strconv.Itoa(root.Addr().(*net.TCPAddr).Port)
	ip := conn.RemoteAddr().(*net.TCPAddr).IP.String()

	// fire a request to send a banner if the attacker does not send first
	bannerCtx, bannerCancel := context.WithCancel(context.Background())
	go s.sendBanner(bannerCtx, muc, uint16(root.Addr().(*net.TCPAddr).Port))

	timeoutCtx, timeoutCancel := context.WithCancel(context.Background())
	go s.timeoutConnection(timeoutCtx, muc)

	// How are those first bytes tasting?
	n := 1500
	buf := make([]byte, n)
	n, err := r.Read(buf)
	if err != nil {
		if err != io.EOF {
			s.logger.Debug().Err(err).
				Str("network", "tcp").
				Msg("error reading from sniffer")
			muc.Close()
		}
	}
	bannerCancel()  // Cancel the banner
	timeoutCancel() // Cancel the timeout

	tlsUnwrap := false
	// Try unwrapping TLS/SSL
	if buf[0] == 0x16 {
		muc.DoneSniffing()
		newMuxConn, newBuf, newN, err := s.unwrapTLS(muc)
		if err == nil {
			muc = newMuxConn
			buf = newBuf
			n = newN
			tlsUnwrap = true
		}
	}
	muc.Reset()
	// get the hash of the first n bytes and tag the context
	hash := drivers.GetHash(buf[:n])
	muc.Context = context.WithValue(muc.Context, gctx.HashContextKey, hash)

	// log the connection
	attacklog := gctx.GetLoggerFromContext(muc.Context).With().Bool("tlsunwrap", tlsUnwrap).Str("network", "tcp").Str("attacker", ip).Str("uuid", muc.GetUUID()).Str("dstport", port).Str("hash", hash).Logger()
	muc.Context = context.WithValue(muc.Context, gctx.LoggerContextKey, attacklog)
	attacklog.Trace().Msgf("tcp knock")

	// save the raw data
	if n > 0 {
		if _, ok := s.knownHashes.Load(hash); !ok {
			s.storeChan <- store.File{Filename: hash, Location: "raw", Data: buf[:n]}
		}
	}

	// see if we match a rule and transfer the connection to the driver
	entry := s.tcpRules.Match(buf)

	// stop sniffing and pass to the driver listener
	muc.Reset()
	ln, ok := entry.(muxconn.MuxListener)
	if ok {
		// hack in the source listener
		ln.Listener = root
		// pipe the connection into Accept()
		ln.ConnCh <- muc
	} else {
		// no driver
		if n > 0 {
			attacklog.Debug().Err(err).Msg("no driver")
		}

		// close the connection
		muc.Close()
	}
}
