package libproxy

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const (
	maxBufferSize = 65536
)

type windowState struct {
	current uint64
	allowed uint64
}

func (w *windowState) String() string {
	return fmt.Sprintf("current %d, allowed %d", w.current, w.allowed)
}

func newWindowState() *windowState {
	return &windowState{}
}
func (w *windowState) size() int {
	return int(w.allowed - w.current)
}
func (w *windowState) isAlmostClosed() bool {
	return w.size() < maxBufferSize/2
}
func (w *windowState) advance() {
	w.allowed = w.current + uint64(maxBufferSize)
}

type channel struct {
	m             *sync.Mutex
	c             *sync.Cond
	multiplexer   *Multiplexer
	destination   Destination
	ID            uint32
	read          *windowState
	write         *windowState
	readPipe      *bufferedPipe
	closeReceived bool
	closeSent     bool
	shutdownSent  bool
	writeDeadline time.Time
}

// newChannel registers a channel through the multiplexer
func newChannel(multiplexer *Multiplexer, ID uint32, d Destination) *channel {
	var m sync.Mutex
	c := sync.NewCond(&m)
	readPipe := newBufferedPipe()
	return &channel{
		m:           &m,
		c:           c,
		multiplexer: multiplexer,
		destination: d,
		ID:          ID,
		read:        &windowState{},
		write:       &windowState{},
		readPipe:    readPipe,
	}
}

func (c *channel) sendWindowUpdate() error {
	c.m.Lock()
	c.read.advance()
	seq := c.read.allowed
	c.m.Unlock()
	return c.multiplexer.send(NewWindow(c.ID, seq))
}

func (c *channel) recvWindowUpdate(seq uint64) {
	c.m.Lock()
	c.write.allowed = seq
	c.c.Signal()
	c.m.Unlock()
}

func (c *channel) Read(p []byte) (int, error) {
	n, err := c.readPipe.Read(p)
	c.m.Lock()
	c.read.current = c.read.current + uint64(n)
	needUpdate := c.read.isAlmostClosed()
	c.m.Unlock()
	if needUpdate {
		c.sendWindowUpdate()
	}
	return n, err
}

func (c *channel) Write(p []byte) (int, error) {
	c.m.Lock()
	defer c.m.Unlock()
	written := 0
	for {
		if len(p) == 0 {
			return written, nil
		}
		if c.closeReceived || c.closeSent || c.shutdownSent {
			return written, io.EOF
		}
		if c.write.size() > 0 {
			toWrite := c.write.size()
			if toWrite > len(p) {
				toWrite = len(p)
			}
			// need to write the header and the payload together
			c.multiplexer.writeMutex.Lock()
			f := NewData(c.ID, uint32(toWrite))
			err1 := f.Write(c.multiplexer.connW)
			_, err2 := c.multiplexer.connW.Write(p[0:toWrite])
			err3 := c.multiplexer.connW.Flush()
			c.multiplexer.writeMutex.Unlock()

			if err1 != nil {
				return written, err1
			}
			if err2 != nil {
				return written, err2
			}
			if err3 != nil {
				return written, err3
			}
			c.write.current = c.write.current + uint64(toWrite)
			p = p[toWrite:]
			written = written + toWrite
			continue
		}

		// Wait for the write window to be increased (or a timeout)
		done := make(chan struct{})
		timeout := make(chan time.Time)
		if !c.writeDeadline.IsZero() {
			go func() {
				time.Sleep(time.Until(c.writeDeadline))
				close(timeout)
			}()
		}
		go func() {
			c.c.Wait()
			close(done)
		}()
		select {
		case <-timeout:
			// clean up the goroutine
			c.c.Broadcast()
			<-done
			return written, &errTimeout{}
		case <-done:
			// The timeout will still fire in the background
			continue
		}
	}
}

func (c *channel) Close() error {
	if err := c.multiplexer.send(NewClose(c.ID)); err != nil {
		return err
	}
	c.m.Lock()
	defer c.m.Unlock()
	c.closeSent = true
	c.c.Broadcast()
	if c.closeSent && c.closeReceived {
		c.multiplexer.freeChannel(c.ID)
	}
	return nil
}

func (c *channel) CloseRead() error {
	return c.readPipe.CloseWrite()
}

func (c *channel) CloseWrite() error {
	if err := c.multiplexer.send(NewShutdown(c.ID)); err != nil {
		return err
	}
	c.m.Lock()
	defer c.m.Unlock()
	c.shutdownSent = true
	c.c.Broadcast()
	return nil
}

func (c *channel) recvClose() error {
	c.m.Lock()
	defer c.m.Unlock()
	c.closeReceived = true
	c.c.Broadcast()
	return nil
}

func (c *channel) isClosed() bool {
	c.m.Lock()
	defer c.m.Unlock()
	return c.closeReceived && c.closeSent
}

func (c *channel) SetReadDeadline(timeout time.Time) error {
	return c.readPipe.SetReadDeadline(timeout)
}

func (c *channel) SetWriteDeadline(timeout time.Time) error {
	c.m.Lock()
	defer c.m.Unlock()
	c.writeDeadline = timeout
	c.c.Broadcast()
	return nil
}

func (c *channel) SetDeadline(timeout time.Time) error {
	if err := c.SetReadDeadline(timeout); err != nil {
		return err
	}
	return c.SetWriteDeadline(timeout)
}

func (c *channel) RemoteAddr() net.Addr {
	return &channelAddr{
		d: c.destination,
	}
}

func (c *channel) LocalAddr() net.Addr {
	return c.RemoteAddr() // There is no local address
}

type channelAddr struct {
	d Destination
}

func (a *channelAddr) Network() string {
	return "channel"
}

func (a *channelAddr) String() string {
	return a.d.String()
}

// Multiplexer muxes and demuxes sub-connections over a single connection
type Multiplexer struct {
	label         string
	conn          io.Closer
	connR         io.Reader // with buffering
	connW         *bufio.Writer
	writeMutex    *sync.Mutex // hold when writing on the channel
	channels      map[uint32]*channel
	nextChannelID uint32
	metadataMutex *sync.Mutex // hold when reading/modifying this structure
	pendingAccept []*channel  // incoming connections
	acceptCond    *sync.Cond
}

// NewMultiplexer constructs a multiplexer from a channel
func NewMultiplexer(label string, conn io.ReadWriteCloser) *Multiplexer {
	var writeMutex, metadataMutex sync.Mutex
	acceptCond := sync.NewCond(&metadataMutex)
	channels := make(map[uint32]*channel)
	connR := bufio.NewReader(conn)
	connW := bufio.NewWriter(conn)
	return &Multiplexer{
		label:         label,
		conn:          conn,
		connR:         connR,
		connW:         connW,
		writeMutex:    &writeMutex,
		channels:      channels,
		metadataMutex: &metadataMutex,
		acceptCond:    acceptCond,
	}
}

func (m *Multiplexer) send(f *Frame) error {
	m.writeMutex.Lock()
	defer m.writeMutex.Unlock()
	if err := f.Write(m.connW); err != nil {
		return err
	}
	return m.connW.Flush()
}

func (m *Multiplexer) findFreeChannelID() uint32 {
	// the metadataMutex is already held
	id := m.nextChannelID
	for {
		if _, ok := m.channels[id]; !ok {
			m.nextChannelID = id + 1
			return id
		}
		id++
	}
}

func (m *Multiplexer) freeChannel(ID uint32) {
	m.metadataMutex.Lock()
	defer m.metadataMutex.Unlock()
	delete(m.channels, ID)
}

// Dial opens a connection to the given destination
func (m *Multiplexer) Dial(d Destination) (Conn, error) {
	m.metadataMutex.Lock()
	id := m.findFreeChannelID()
	channel := newChannel(m, id, d)
	m.channels[id] = channel
	m.metadataMutex.Unlock()

	if err := m.send(NewOpen(id, d)); err != nil {
		return nil, err
	}
	if err := channel.sendWindowUpdate(); err != nil {
		return nil, err
	}
	return channel, nil
}

// Accept returns the next client connection
func (m *Multiplexer) Accept() (Conn, *Destination, error) {
	m.metadataMutex.Lock()
	defer m.metadataMutex.Unlock()
	for {
		if len(m.pendingAccept) > 0 {
			first := m.pendingAccept[0]
			m.pendingAccept = m.pendingAccept[1:]
			if err := first.sendWindowUpdate(); err != nil {
				return nil, nil, err
			}
			return first, &first.destination, nil
		}
		m.acceptCond.Wait()
	}
}

// Run starts handling the requests from the other side
func (m *Multiplexer) Run() {
	go func() {
		if err := m.run(); err != nil {
			log.Printf("Multiplexer main loop failed with %v", err)
		}
	}()
}

func (m *Multiplexer) run() error {
	for {
		f, err := unmarshalFrame(m.connR)
		if err != nil {
			return fmt.Errorf("Failed to unmarshal command frame: %v", err)
		}
		switch f.Command {
		case Open:
			o, err := f.Open()
			if err != nil {
				return fmt.Errorf("Failed to unmarshal open command: %v", err)
			}
			switch o.Connection {
			case Dedicated:
				return fmt.Errorf("Dedicated connections are not implemented yet")
			case Multiplexed:
				m.metadataMutex.Lock()
				channel := newChannel(m, f.ID, o.Destination)
				m.channels[f.ID] = channel
				m.pendingAccept = append(m.pendingAccept, channel)
				m.acceptCond.Signal()
				m.metadataMutex.Unlock()
			}
		case Window:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %v", f.ID)
			}
			w, err := f.Window()
			if err != nil {
				return err
			}
			channel.recvWindowUpdate(w.seq)
		case Data:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %v", f.ID)
			}
			d, err := f.Data()
			if err != nil {
				return err
			}
			if _, err := io.CopyN(channel.readPipe, m.connR, int64(d.payloadlen)); err != nil {
				return err
			}
		case Shutdown:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %v", f.ID)
			}
			if err := channel.readPipe.CloseWrite(); err != nil {
				return err
			}
		case Close:
			m.metadataMutex.Lock()
			channel, ok := m.channels[f.ID]
			m.metadataMutex.Unlock()
			if !ok {
				return fmt.Errorf("Unknown channel id: %v", f.ID)
			}
			// this will unblock waiting Read calls
			if err := channel.readPipe.CloseWrite(); err != nil {
				return err
			}
			// this will unblock waiting Write calls
			if err := channel.recvClose(); err != nil {
				return err
			}
			if channel.isClosed() {
				m.freeChannel(channel.ID)
			}
		default:
			return fmt.Errorf("Unknown command type: %v", f)
		}
	}
}

// Forward runs the TCP/UDP forwarder over a sub-connection
func Forward(conn Conn, destination Destination, quit chan struct{}) {
	defer conn.Close()

	switch destination.Proto {
	case TCP:
		backendAddr := net.TCPAddr{IP: destination.IP, Port: int(destination.Port), Zone: ""}
		if err := HandleTCPConnection(conn, &backendAddr, quit); err != nil {
			log.Printf("Error setting up TCP proxy subconnection: %v", err)
			return
		}
	case Unix:
		backendAddr, err := net.ResolveUnixAddr("unix", destination.Path)
		if err != nil {
			log.Printf("Error resolving Unix address %s", destination.Path)
			return
		}
		if err := HandleUnixConnection(conn, backendAddr, quit); err != nil {
			log.Printf("Error setting up Unix proxy subconnection: %v", err)
			return
		}
	case UDP:
		backendAddr := &net.UDPAddr{IP: destination.IP, Port: int(destination.Port), Zone: ""}

		proxy, err := NewUDPProxy(backendAddr, NewUDPConn(conn), backendAddr)
		if err != nil {
			log.Printf("Failed to setup UDP proxy for %s: %#v", backendAddr, err)
			return
		}
		proxy.Run()
		return
	default:
		log.Printf("Unknown protocol: %d", destination.Proto)
		return
	}
}
