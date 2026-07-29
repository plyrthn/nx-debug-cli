package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// defaultGdbPort is IANA's gdb-remote port. Using the conventional one means
// a launch config someone already has probably works unchanged.
const defaultGdbPort = 2159

// cmdGdb forwards the target's gdb stub to a fixed local port and says, in
// as few words as possible, how to attach to it.
//
// The stub is already published by `nxdbg serve`, but on a port the target
// picks, which is no use in a saved launch configuration. Pinning it to a
// known port is the difference between "there is a gdb server somewhere" and
// something a person can paste into their debugger.
func cmdGdb(ctx context.Context, serial string, rest []string) error {
	port := defaultGdbPort
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-p", "--port":
			if i+1 >= len(rest) {
				return fmt.Errorf("usage: nxdbg gdb <serial> [--port N]")
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				return fmt.Errorf("invalid port %q: %w", rest[i+1], err)
			}
			port, i = n, i+1
		default:
			return fmt.Errorf("unknown option %q", rest[i])
		}
	}

	entry, err := htc.ResolvePort(ctx, serial, "gdb")
	if err != nil {
		return err
	}
	target := entry.Addr()

	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", local)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w (try --port with a free one)", local, err)
	}
	defer ln.Close()

	printGdbBanner(local, serial)

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	go func() {
		<-sigCtx.Done()
		ln.Close()
	}()

	for {
		client, err := ln.Accept()
		if err != nil {
			if sigCtx.Err() != nil {
				fmt.Println("\nstopped")
				return nil
			}
			return err
		}
		fmt.Printf("✓ debugger attached from %s\n", client.RemoteAddr())
		go proxyGdb(client, target)
	}
}

// proxyGdb splices one debugger session onto the target's stub. Each attach
// gets a fresh connection to the stub, since a debugger that detaches and
// reattaches expects a clean session rather than someone else's leftovers.
func proxyGdb(client net.Conn, target string) {
	defer client.Close()
	up, err := net.Dial("tcp", target)
	if err != nil {
		fmt.Printf("✗ could not reach the gdb stub at %s: %v\n", target, err)
		return
	}
	defer up.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(up, client); up.Close() }()
	go func() { defer wg.Done(); io.Copy(client, up); client.Close() }()
	wg.Wait()
	fmt.Println("  debugger detached")
}

// printGdbBanner is deliberately loud. The whole point of this command is
// that someone who has never seen this tool can attach their own debugger
// without reading anything else.
func printGdbBanner(addr, serial string) {
	line := "----------------------------------------------------------------"
	fmt.Println()
	fmt.Println(line)
	fmt.Printf("  GDB SERVER READY   -   target %s\n", serial)
	fmt.Println(line)
	fmt.Println()
	fmt.Printf("  Listening on  %s\n", addr)
	fmt.Println("  Architecture  aarch64 (ARMv8-A, little endian)")
	fmt.Println()
	fmt.Println("  Attach with whichever of these you already use:")
	fmt.Println()
	fmt.Println("    gdb       aarch64-none-elf-gdb  (or gdb-multiarch)")
	fmt.Printf("                (gdb) target extended-remote %s\n", addr)
	fmt.Println()
	fmt.Println("    lldb      lldb")
	fmt.Printf("                (lldb) gdb-remote %s\n", addr)
	fmt.Println()
	fmt.Println("    VS Code   in launch.json, with the cppdbg type:")
	fmt.Println(`                "MIMode": "gdb",`)
	fmt.Printf("                %q: %q\n", "miDebuggerServerAddress", addr)
	fmt.Println()
	fmt.Println("    IDA       Debugger > Remote GDB debugger, then Process > Attach")
	fmt.Printf("                hostname 127.0.0.1, port %s\n", portOf(addr))
	fmt.Println()
	fmt.Println("    Ghidra    Debugger > Connect > gdb, remote target")
	fmt.Printf("                target extended-remote %s\n", addr)
	fmt.Println()
	fmt.Println("  This needs no SDK and no daemon: it is the target's own stub,")
	fmt.Println("  reached over the link this tool is already driving.")
	fmt.Println()
	fmt.Println("  Ctrl-C to stop.")
	fmt.Println(line)
	fmt.Println()
}

func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return p
}
