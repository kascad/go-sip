package sipnet

import (
	"errors"
	"net"
	"sync"
	"time"
)

var (
	// ErrClosed is returned if AcceptRequest is called on a closed listener.
	// io.EOF may also be returned on a closed underlying connection, in which
	// the connection itself will also be returned.
	ErrClosed        = errors.New("sip: closed")
	ErrInvalidBranch = errors.New("sip: invalid branch")
)

type requestPackage struct {
	conn *Conn
	req  *Request
	err  error
}

// Listener represents a TCP and UDP wrapper listener.
type Listener struct {
	tcpListener net.Listener
	udpListener *net.UDPConn
	closed      bool

	requestChannel   chan requestPackage
	receivedBranches map[string]time.Time
	branchMutex      *sync.Mutex

	udpPool      map[string]*Conn
	udpPoolMutex *sync.Mutex
}

// Listen listens on an address (IP:port) on both TCP and UDP.
func Listen(addr string) (*Listener, error) {
	tcpListener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		tcpListener.Close()
		return nil, err
	}

	udpListener, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		tcpListener.Close()
		return nil, err
	}

	listener := &Listener{
		tcpListener:      tcpListener,
		udpListener:      udpListener,
		closed:           false,
		requestChannel:   make(chan requestPackage),
		receivedBranches: make(map[string]time.Time),
		branchMutex:      new(sync.Mutex),
	}

	go branchJanitor(listener)
	go handleTCPListening(listener)
	go handleUDPListening(listener)

	return listener, nil
}

func branchJanitor(listener *Listener) {
	for {
		time.Sleep(time.Second * 10)
		if listener.closed {
			return
		}

		listener.branchMutex.Lock()
		for branch, t := range listener.receivedBranches {
			if time.Now().Sub(t) > 30*time.Second {
				delete(listener.receivedBranches, branch)
			}
		}
		listener.branchMutex.Unlock()
	}
}

func handleTCPListening(listener *Listener) {
	defer listener.Close()

	for {
		conn, err := listener.tcpListener.Accept()
		if err != nil {
			if listener.closed {
				return
			}

			listener.requestChannel <- requestPackage{
				conn: nil,
				req:  nil,
				err:  err,
			}

			return
		}

		listener.registerTCPConn(conn)
	}
}

func handleUDPListening(listener *Listener) {
	defer listener.Close()

	for {
		data := make([]byte, 65535)
		n, addr, err := listener.udpListener.ReadFrom(data)
		if err != nil {
			if listener.closed {
				return
			}

			listener.requestChannel <- requestPackage{
				conn: nil,
				req:  nil,
				err:  err,
			}

			return
		}

		listener.getUDPConnFromPool(addr).writeReceivedUDP(data[:n])
	}
}

// AcceptRequest blocks until it receives a Request message on either TCP or UDP
// listeners. Responses are to be written to *Conn (and then flushed).
func (l *Listener) AcceptRequest() (*Request, *Conn, error) {
	for {
		if l.closed {
			return nil, nil, ErrClosed
		}
		resp := <-l.requestChannel

		if resp.err == nil {
			via, err := ParseVia(resp.req.Header.Get("Via"))
			if err != nil {
				return resp.req, resp.conn, err
			}

			branch := via.Arguments.Get("branch")
			if branch == "" || len(branch) < 8 || branch[:7] != "z9hG4bK" {
				return resp.req, resp.conn, ErrInvalidBranch
			}

			l.branchMutex.Lock()
			if _, found := l.receivedBranches[branch]; found {
				// Repeated message, ignore.
				l.branchMutex.Unlock()
				continue
			}

			l.receivedBranches[branch] = time.Now()
			l.branchMutex.Unlock()
		}

		return resp.req, resp.conn, resp.err
	}
}

// Close closes both TCP and UDP listeners, and returns
func (l *Listener) Close() error {
	l.closed = true
	err := l.tcpListener.Close()
	if err != nil {
		l.udpListener.Close()
	} else {
		err = l.udpListener.Close()
	}

closeLoop:
	for {
		select {
		case <-l.requestChannel:
		default:
			break closeLoop
		}
	}

	return err
}

// Addr returns the address the listener is listening on.
func (l *Listener) Addr() net.Addr {
	return l.tcpListener.Addr()
}
