package runtimeenrollment

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/redis/go-redis/v9"
)

func TestRedisClientProductionOptionsAreBoundedAndIndependent(t *testing.T) {
	client := newRedisClient("127.0.0.1:6380").(*redis.Client)
	defer client.Close()
	o := client.Options()
	if o.Addr != "127.0.0.1:6380" || o.Protocol != 2 || !o.DisableIdentity || !o.ContextTimeoutEnabled || o.MaxRetries != 0 ||
		o.DialTimeout > 2*time.Second || o.ReadTimeout > 2*time.Second || o.WriteTimeout > 2*time.Second ||
		o.Username != "" || o.Password != "" || o.DB != 0 {
		t.Fatal("unexpected production Redis client configuration")
	}
}

// This exercises the actual go-redis transport against a loopback RESP endpoint,
// not a RedisCommander mock. The endpoint has no execution lease to grant.
func TestDependenciesActualRedisWireOnlyPingsAndValidates(t *testing.T) {
	database, repository, c := enrollmentDependenciesFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var mu sync.Mutex
	var commands [][]string
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		for {
			command, err := readTestRESPCommand(reader)
			if err != nil {
				return
			}
			mu.Lock()
			commands = append(commands, command)
			mu.Unlock()
			switch strings.ToLower(command[0]) {
			case "hello":
				_, _ = io.WriteString(conn, "-ERR unknown command 'hello'\r\n")
			case "ping":
				_, _ = io.WriteString(conn, "+PONG\r\n")
			case "eval":
				_, _ = io.WriteString(conn, ":0\r\n")
			default:
				_, _ = io.WriteString(conn, "-ERR forbidden command\r\n")
			}
		}
	}()
	c.LeaseRedisAddr = listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d, err := New(ctx, c, database, repository)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	claim := lease.Claim{SlotID: "slot-a", NodeID: "node-a", ExecutionEpoch: 1, OwnerID: "owner-a"}
	if err := d.ControlConfig().Leases.Validate(ctx, claim); err != lease.ErrLeaseNotCurrent {
		t.Fatalf("missing live Redis lease accepted: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("owned Redis transport remained open")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 3 || strings.ToLower(commands[0][0]) != "hello" || strings.ToLower(commands[1][0]) != "ping" || strings.ToLower(commands[2][0]) != "eval" {
		t.Fatal("unexpected Redis commands during production dependency startup/validation")
	}
	validation := commands[2]
	if len(validation) != 5 || validation[2] != "1" || !strings.HasPrefix(validation[3], config.RuntimeLeaseKeyPrefix) ||
		!strings.Contains(validation[1], "'GET'") || strings.Contains(validation[1], "'SET'") || strings.Contains(validation[1], "'DEL'") {
		t.Fatal("actual Redis transport used wrong namespace or wrote a lease")
	}
}

func readTestRESPCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil || len(line) < 4 || line[0] != '*' {
		return nil, fmt.Errorf("invalid test RESP command")
	}
	count, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "*"), "\r\n"))
	if err != nil || count < 1 || count > 10 {
		return nil, fmt.Errorf("invalid test RESP argument count")
	}
	command := make([]string, count)
	for index := range command {
		line, err = reader.ReadString('\n')
		if err != nil || len(line) < 4 || line[0] != '$' {
			return nil, fmt.Errorf("invalid test RESP argument")
		}
		size, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "$"), "\r\n"))
		if err != nil || size < 0 || size > 4096 {
			return nil, fmt.Errorf("invalid test RESP argument size")
		}
		data := make([]byte, size+2)
		if _, err := io.ReadFull(reader, data); err != nil || string(data[size:]) != "\r\n" {
			return nil, fmt.Errorf("invalid test RESP argument body")
		}
		command[index] = string(data[:size])
	}
	return command, nil
}
