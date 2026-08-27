package harness

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

type loopbackTCPFixture struct {
	listener *net.TCPListener
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	closeErr error
}

func startLoopbackTCPFixture() (*loopbackTCPFixture, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	fixture := &loopbackTCPFixture{listener: listener, done: make(chan struct{})}
	go func() {
		defer close(fixture.done)
		for {
			connection, err := listener.AcceptTCP()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				fixture.setError(fmt.Errorf("accept loopback connection: %w", err))
				return
			}
			if remote, ok := connection.RemoteAddr().(*net.TCPAddr); !ok || !remote.IP.IsLoopback() {
				fixture.setError(fmt.Errorf("TCP fixture accepted non-loopback peer %v", connection.RemoteAddr()))
			}
			_ = connection.Close()
		}
	}()
	return fixture, nil
}

func (fixture *loopbackTCPFixture) Port() int {
	if fixture == nil || fixture.listener == nil {
		return 0
	}
	return fixture.listener.Addr().(*net.TCPAddr).Port
}

func (fixture *loopbackTCPFixture) Close() error {
	if fixture == nil {
		return nil
	}
	fixture.once.Do(func() {
		if err := fixture.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			fixture.setError(fmt.Errorf("close loopback listener: %w", err))
		}
		<-fixture.done
	})
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.closeErr
}

func (fixture *loopbackTCPFixture) setError(err error) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.closeErr == nil {
		fixture.closeErr = err
	}
}

func applyTCPListenerArgument(arguments []Argument, listener *TCPListener, port int) ([]Argument, error) {
	resolved := make([]Argument, len(arguments))
	copy(resolved, arguments)
	if listener == nil {
		return resolved, nil
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("TCP fixture port %d is outside 1..65535", port)
	}
	if listener.PortArgument < 0 || listener.PortArgument >= len(resolved) {
		return nil, fmt.Errorf("TCP listener port argument %d is outside the argument list", listener.PortArgument)
	}
	if resolved[listener.PortArgument].Type != "int" {
		return nil, fmt.Errorf("TCP listener port argument %d must be int", listener.PortArgument)
	}
	resolved[listener.PortArgument].Uint64 = uint64(port)
	return resolved, nil
}
