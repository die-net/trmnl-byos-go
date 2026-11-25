package render

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/die-net/sandbox"
)

// SelfTestCommand is the trmnl-byos subcommand that trmnl-byos doctor
// runs inside a build's sandbox, with the build's own Spec, environment and
// proxy listener, to check what a build reaches through that listener.
// sandbox.Check cannot: it makes a sandbox of its own, whose listener
// nothing serves.
const SelfTestCommand = "render-selftest"

// targets is a flag that may be given more than once.
type targets []string

func (t *targets) String() string { return strings.Join(*t, ",") }

func (t *targets) Set(v string) error {
	*t = append(*t, v)
	return nil
}

// selfTestTimeout bounds each CONNECT the self-test tries.
const selfTestTimeout = 10 * time.Second

// RunSelfTest runs SelfTestCommand. It sends a CONNECT for each -refuse
// and -allow host:port through the proxy in HTTPS_PROXY, and writes one
// sandbox.Result for each as a JSON line to stdout. It returns the exit
// status.
func RunSelfTest(ctx context.Context, args []string, stdout io.Writer) (int, error) {
	fs := flag.NewFlagSet(SelfTestCommand, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var refuse, allow targets
	fs.Var(&refuse, "refuse", "a host:port the proxy must refuse; repeat for more")
	fs.Var(&allow, "allow", "a host:port the proxy must allow; repeat for more")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage, fmt.Errorf("usage: %s [-refuse host:port] [-allow host:port], each repeated as needed", SelfTestCommand)
	}
	proxy := strings.TrimPrefix(os.Getenv("HTTPS_PROXY"), "http://")
	if proxy == "" {
		return exitUnavailable, errors.New("no HTTPS_PROXY")
	}
	enc := json.NewEncoder(stdout)
	for _, t := range []struct {
		targets []string
		allowed bool
	}{{refuse, false}, {allow, true}} {
		for _, target := range t.targets {
			if err := enc.Encode(connectResult(ctx, proxy, target, t.allowed)); err != nil {
				return exitUnavailable, err
			}
		}
	}
	return 0, nil
}

// connectResult sends CONNECT target to the proxy at addr, and says whether
// it was answered as it should be: with 200 when allowed, and with a
// refusal otherwise.
func connectResult(ctx context.Context, addr, target string, allowed bool) sandbox.Result {
	name := "proxy.refused:" + target
	if allowed {
		name = "proxy.allowed:" + target
	}
	code, err := connect(ctx, addr, target)
	switch {
	case err != nil:
		return sandbox.Result{Name: name, Status: sandbox.Fail, Detail: fmt.Sprintf("CONNECT %s through %s: %v", target, addr, err)}
	case allowed == (code == http.StatusOK):
		return sandbox.Result{Name: name, Status: sandbox.Pass, Detail: fmt.Sprintf("CONNECT %s through %s: status %d", target, addr, code)}
	case allowed:
		return sandbox.Result{Name: name, Status: sandbox.Fail, Detail: fmt.Sprintf("CONNECT %s through %s: status %d, so builds cannot reach it", target, addr, code)}
	default:
		return sandbox.Result{Name: name, Status: sandbox.Fail, Detail: fmt.Sprintf("CONNECT %s through %s: status %d, so builds can reach it", target, addr, code)}
	}
}

// connect sends CONNECT target to the HTTP proxy at addr and returns the
// status it answered.
func connect(ctx context.Context, addr, target string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
