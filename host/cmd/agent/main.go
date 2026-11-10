// Command pack-host-agent is the process pack-target-host runs on a managed host: it listens on the loopback,
// speaks pack.agent.v1 (proto/agent.proto) and does what the driver asks — runs commands, delivers files, serves
// the files of an OCI artifact over HTTP. Its lifetime is the session's: EOF on stdin is its signal to exit.
//
//	pack-host-agent -addr 127.0.0.1:0 -token <the driver's token>
//
// The chosen address is printed on stdout's first line, "listening <addr>", which is how the driver learns it.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"google.golang.org/grpc"

	"github.com/horchestra/targets/host/internal/agent"
	"github.com/horchestra/targets/host/internal/agentpb"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "the loopback address to listen on")
	token := flag.String("token", "", "the token every call must carry as the metadata pack-agent-token")
	flag.Parse()
	if err := run(*addr, *token); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "pack-host-agent:", err)
		os.Exit(1)
	}
}

func run(addr, token string) error {
	if len(token) == 0 {
		return fmt.Errorf("-token is required")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv, err := agent.NewServer(token)
	if err != nil {
		return err
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(agent.AuthUnary(token)), grpc.StreamInterceptor(agent.AuthStream(token)))
	agentpb.RegisterAgentServer(s, srv)
	// the address first: the driver reads this very line
	if _, err := fmt.Fprintf(os.Stdout, "listening %s\n", ln.Addr()); err != nil {
		return err
	}
	// stdin's EOF is the session's end: stop, the requests under way finished
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		s.GracefulStop()
	}()
	return s.Serve(ln)
}
