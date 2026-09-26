package localhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	systemdDest    = "org.freedesktop.systemd1"
	systemdManager = "org.freedesktop.systemd1.Manager"
	systemdUnit    = "org.freedesktop.systemd1.Unit"
	systemdService = "org.freedesktop.systemd1.Service"
)

// liveSystem is systemd over the system bus and journalctl on this machine.
type liveSystem struct{}

// connect opens the system bus for as long as the module runs; a machine not booted with
// systemd has no /run/systemd/system.
func (liveSystem) connect(context.Context) (unitSource, error) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return nil, errNoSystemd
	}
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, err
	}
	return &busUnits{conn: conn}, nil
}

type busUnits struct{ conn *dbus.Conn }

func (b *busUnits) units(ctx context.Context) ([]unitReply, error) {
	var out []unitReply
	err := b.conn.Object(systemdDest, "/org/freedesktop/systemd1").
		CallWithContext(ctx, systemdManager+".ListUnits", 0).Store(&out)
	return out, err
}

// details reads the Unit properties, and a service's Service properties.
func (b *busUnits) details(ctx context.Context, u unitReply) (unitProps, error) {
	ifaces := []string{systemdUnit}
	if _, typ := unitKind(u.Name); typ == "service" {
		ifaces = append(ifaces, systemdService)
	}
	props := map[string]dbus.Variant{}
	for _, iface := range ifaces {
		var some map[string]dbus.Variant
		err := b.conn.Object(systemdDest, u.Path).
			CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, iface).Store(&some)
		if err != nil {
			return unitProps{}, fmt.Errorf("%s: %w", u.Name, err)
		}
		maps.Copy(props, some)
	}
	return propsFrom(func(name string) any { return props[name].Value() }), nil
}

func (b *busUnits) close() { _ = b.conn.Close() }

// journal starts journalctl; Close waits for it and reports its complaint, if any.
func (liveSystem) journal(ctx context.Context, args []string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.WaitDelay = 500 * time.Millisecond
	stderr := &capped{max: 512}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &journalProc{ReadCloser: out, cmd: cmd, stderr: stderr}, nil
}

type journalProc struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *capped
}

func (p *journalProc) Close() error {
	_ = p.ReadCloser.Close()
	err := p.cmd.Wait()
	if msg := p.stderr.firstLine(); err != nil && msg != "" {
		return errors.New(msg)
	}
	return err
}

// capped keeps the start of what is written to it.
type capped struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (c *capped) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(b[:min(len(b), c.max-c.buf.Len())])
	return len(b), nil
}

func (c *capped) firstLine() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	line, _, _ := strings.Cut(c.buf.String(), "\n")
	return strings.TrimSpace(line)
}
