package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aybabtme/rgbterm"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/net/context"
)

func init() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, `proxyssh: proxy connections over ssh to multiple hosts

FLAGS:`)
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
EXAMPLE:
  Proxy as user 'root' on remote port '3306', for 3 MySQL hosts.
    proxyssh -u root -rp 3306 mysql01.prod.com mysql02.prod.com mysql03.prod.com
`)
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("proxyssh: ")

	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}

	username := flag.String("u", u.Username, "username to use to connect on remote hosts")
	port := flag.Int("p", 22, "ssh port on which to connect")
	remoteport := flag.Int("rp", 80, "remote port on which to proxy")
	flag.Parse()

	if flag.NArg() < 1 {
		log.Fatal("need at least one hostname")
	}

	var hostnames []string
	for _, host := range flag.Args() {
		hostnames = append(hostnames, strings.TrimSpace(host))
	}

	log.Printf("connecting with user %q", *username)
	config, done, err := setupSSHConfig(*username)
	if err != nil {
		log.Fatalf("setting up config: %v", err)
	}
	defer done()

	clients, err := NewClients(hostnames, *port, config)
	if err != nil {
		log.Fatalf("setting up clients: %v", err)
	}
	defer clients.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 2)
	go func() {
		<-sig
		cancel()
	}()
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)

	if err := clients.Proxy(ctx, *remoteport); err != nil {
		log.Printf("proxying to clients: %v", err)
	}
}

func setupSSHConfig(username string) (*ssh.ClientConfig, func(), error) {
	conn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return nil, nil, err
	}

	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeysCallback(agent.NewClient(conn).Signers),
		},
	}

	return config, func() { conn.Close() }, nil
}

type sshclient struct {
	*ssh.Client
	Hostname string
}

type Clients struct {
	clients []*sshclient
}

func NewClients(hostnames []string, port int, config *ssh.ClientConfig) (*Clients, error) {

	clientc := make(chan *sshclient, len(hostnames))
	errc := make(chan error, len(hostnames))

	wg := sync.WaitGroup{}
	for _, hostname := range hostnames {
		wg.Add(1)
		go func(hostname string) {
			defer wg.Done()
			addr := fmt.Sprintf("%s:%d", hostname, port)
			client, err := ssh.Dial("tcp", addr, config)
			if err != nil {
				errc <- err
			} else {
				clientc <- &sshclient{
					Client:   client,
					Hostname: hostname,
				}
			}
		}(hostname)

	}
	wg.Wait()
	close(clientc)
	close(errc)

	clients := &Clients{}
	for c := range clientc {
		clients.clients = append(clients.clients, c)
	}

	for err := range errc {
		clients.AllDo(func(c *sshclient) error {
			return c.Close()
		})
		return nil, err
	}
	return clients, nil
}

func (c *Clients) Close() error {
	return c.AllDo(func(cli *sshclient) error {
		return cli.Close()
	})
}

func (c *Clients) AllDo(f func(*sshclient) error) error {
	errc := make(chan error, len(c.clients))
	wg := sync.WaitGroup{}
	for _, client := range c.clients {
		wg.Add(1)
		go func(c *sshclient) {
			defer wg.Done()
			if err := f(c); err != nil {
				errc <- err
			}
		}(client)
	}
	wg.Wait()
	close(errc)
	return <-errc
}

func (c *Clients) Proxy(ctx context.Context, port int) error {
	rand.Seed(time.Now().Unix())
	return c.AllDo(func(cli *sshclient) error {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		go func() {
			<-ctx.Done()
			l.Close()
		}()

		_, sport, _ := net.SplitHostPort(l.Addr().String())
		prefix := rgbterm.BgString(
			fmt.Sprintf("(%s) %s", sport, cli.Hostname),
			uint8(rand.Intn(255)), uint8(rand.Intn(255)), uint8(rand.Intn(255)),
		)
		llog := log.New(os.Stderr, prefix+": ", 0)

		raddr := fmt.Sprintf("127.0.0.1:%d", port)
		llog.Printf("proxy started on %q", l.Addr())
		for {
			lconn, err := l.Accept()
			if err != nil {
				return err
			}
			defer lconn.Close()
			rconn, err := cli.Dial("tcp", raddr)
			if err != nil {
				return err
			}
			defer rconn.Close()
			go func() {
				llog.Printf("connection started...")
				if err := proxyConn(ctx, lconn, rconn); err != nil {
					llog.Printf("error proxying %q", l.Addr())
				}
				llog.Printf("connection finished...")
			}()
		}
	})
}

func proxyConn(parent context.Context, lconn, rconn net.Conn) error {

	errc := make(chan error, 2)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	closed := false
	go func() {
		defer cancel()
		for !closed {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, err := io.CopyN(rconn, lconn, 8*1<<10)
			if isNormalTerminationError(err) {
				return
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	go func() {
		defer cancel()
		for !closed {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, err := io.CopyN(lconn, rconn, 8*1<<10)
			if isNormalTerminationError(err) {
				return
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}

}

func isNormalTerminationError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return true
	}
	e, ok := err.(*net.OpError)
	if ok && e.Timeout() {
		return true
	}

	for _, cause := range []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
	} {
		if strings.Contains(err.Error(), cause) {
			return true
		}
	}

	return false
}
