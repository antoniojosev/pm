package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/antoniojosev/pm/client"
)

// Reserve ports for a worktree, start a process on them and bring everything
// of that worktree down again. It has no Output line, so `go test` compiles it
// without running it (it needs a pm daemon on 127.0.0.1:9797).
func Example() {
	ctx := context.Background()
	c := client.New("http://127.0.0.1:9797")

	v, err := c.Version(ctx) // fails if the daemon speaks another API generation
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("pm", v.Version, "docker:", v.Docker)

	lease, err := c.Leases.Create(ctx, client.LeaseRequest{Owner: "wt-qa", Count: 2})
	if err != nil {
		log.Fatal(err)
	}

	unit, err := c.Run(ctx, client.RunSpec{
		Name:   "api-qa",
		Dir:    "/home/me/src/api",
		Cmd:    []string{"npm", "run", "dev"},
		Env:    map[string]string{"PORT": fmt.Sprint(lease.Ports[0])},
		Limits: client.Limits{Memory: "2G", CPU: "150%"},
		Owner:  "wt-qa",
		Ports:  lease.Ports,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("started", unit.Unit, "pid", unit.PID)

	out, _ := c.Logs(ctx, "api-qa", 20)
	fmt.Println(out)

	// Everything of the owner stops and its leases are released.
	if _, err := c.Owners.Delete(ctx, "wt-qa"); err != nil {
		log.Fatal(err)
	}
}

// A client refuses a daemon that speaks another API generation, with an error
// you can test for.
func ExampleVersionError() {
	// A stand-in for a daemon from the future.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"3.0.0","api":2,"caddy":false,"docker":false,"systemd":true}`)
	}))
	defer srv.Close()

	c := client.New(srv.URL)
	_, err := c.Leases.Create(context.Background(), client.LeaseRequest{Owner: "wt-qa"})
	fmt.Println(errors.Is(err, client.ErrIncompatible))
	fmt.Println(err)
	// Output:
	// true
	// pm daemon speaks API v2 (pm 3.0.0) but this client speaks API v1: use a client that matches the daemon
}
