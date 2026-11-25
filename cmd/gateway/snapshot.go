package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"

	"google.golang.org/protobuf/proto"

	"github.com/ankitbisht23/spillway/internal/managed"
)

// The on-disk snapshot's two helpers for Kubernetes (CP-07, ADR-0075 §D11,
// docs/runbooks/gateway-snapshot.md); the distroless image has no shell,
// cp or tar.
//
// Neither prints the snapshot's content to the terminal or a log: it holds
// API key hashes. Messages go to stderr; snapshot-export writes the
// snapshot file's bytes to stdout only.

// snapshotSeed is "gateway snapshot-seed --from <file> --to <file>", the
// seed init container: it copies the snapshot at --from to --to when --to
// does not exist yet, after checking that --from parses, is of this
// gateway's schema major and has both sections; it does nothing when --to
// exists or --from does not (the seed Secret is optional and is deleted
// once the control plane is back). It exits 0 when done or when there is
// nothing to do, 1 on a bad or unwritable snapshot and 2 on bad flags.
func snapshotSeed(args []string, stderr io.Writer) int {
	fl := flag.NewFlagSet("gateway snapshot-seed", flag.ContinueOnError)
	fl.SetOutput(stderr)
	from := fl.String("from", "", "the seed snapshot `file` (required)")
	to := fl.String("to", "", "the snapshot `file` to create, SPILLWAY_GATEWAY_SNAPSHOT's (required)")
	if err := fl.Parse(args); err != nil {
		return exitCode(err)
	}
	if *from == "" || *to == "" || fl.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-seed: need --from and --to, and no other arguments")
		return 2
	}
	if _, err := os.Stat(*to); err == nil {
		_, _ = fmt.Fprintf(stderr, "gateway snapshot-seed: %s exists; nothing to do\n", *to)
		return 0
	}
	if _, err := os.Stat(*from); errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintf(stderr, "gateway snapshot-seed: no seed at %s; nothing to do\n", *from)
		return 0
	}
	f, err := managed.ReadSnapshot(*from)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-seed:", err)
		return 1
	}
	if err := managed.WriteSnapshot(*to, f); err != nil {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-seed:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "gateway snapshot-seed: wrote %s: config version %d, identity version %d\n",
		*to, f.GetConfig().GetVersion(), f.GetIdentity().GetVersion())
	return 0
}

// snapshotExport is "gateway snapshot-export [--from <file>]", run with
// kubectl exec in a live pod: it checks the snapshot file (default
// SPILLWAY_GATEWAY_SNAPSHOT's, else /var/lib/spillway/snapshot.pb) as the
// gateway would read it and writes it to stdout. It exits 0 on success, 1
// on a missing or bad snapshot (nothing written) and 2 on bad flags.
func snapshotExport(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("gateway snapshot-export", flag.ContinueOnError)
	fl.SetOutput(stderr)
	from := fl.String("from", cmp.Or(getenv("SPILLWAY_GATEWAY_SNAPSHOT"), managed.DefaultSnapshotPath), "the snapshot `file`")
	if err := fl.Parse(args); err != nil {
		return exitCode(err)
	}
	if fl.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-export: unexpected arguments")
		return 2
	}
	f, err := managed.ReadSnapshot(*from)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-export:", err)
		return 1
	}
	b, err := proto.Marshal(f)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-export:", err)
		return 1
	}
	if _, err := stdout.Write(b); err != nil {
		_, _ = fmt.Fprintln(stderr, "gateway snapshot-export:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "gateway snapshot-export: %s: config version %d, identity version %d, %d bytes\n",
		*from, f.GetConfig().GetVersion(), f.GetIdentity().GetVersion(), len(b))
	return 0
}

func exitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}
