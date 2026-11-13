package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// The snapshots of a managed file: before an overwrite that changes it, the file is copied to <path>.bak.1 and
// the older snapshots shift (<path>.bak.1 → .bak.2, …), the one beyond history_count dropped. They are the
// memory a rollback has where pack keeps no record (state: observe): undoing a file step restores .bak.1. cp -a
// keeps the mode and the owner with the bytes. All of it is one shell so a path of the pack's is quoted, never
// split.

// shq quotes a word for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// rotate shifts the path's snapshots and copies the path to .bak.1; a path that is not there has nothing to
// rotate.
func (c *conn) rotate(ctx context.Context, path string) error {
	if c.history <= 0 {
		return nil
	}
	var b strings.Builder
	p := shq(path)
	fmt.Fprintf(&b, "[ -f %s ] || exit 0; rm -f %s; ", p, shq(fmt.Sprintf("%s.bak.%d", path, c.history)))
	for i := c.history - 1; i >= 1; i-- {
		fmt.Fprintf(&b, "[ ! -f %s ] || mv %s %s; ", shq(fmt.Sprintf("%s.bak.%d", path, i)), shq(fmt.Sprintf("%s.bak.%d", path, i)), shq(fmt.Sprintf("%s.bak.%d", path, i+1)))
	}
	fmt.Fprintf(&b, "cp -a %s %s", p, shq(path+".bak.1"))
	_, err := c.exec(ctx, []string{"sh", "-c", b.String()}, nil, nil, nil)
	return err
}

// stateOf is the sha256 and the mode (as stat prints it) of the path on the host; exists is false when the path
// is not there. GNU and BSD both answer (a host is Linux, this machine may be a Mac).
func (c *conn) stateOf(ctx context.Context, path string) (sum string, mode string, exists bool, err error) {
	out, err := c.exec(ctx, []string{"sh", "-c",
		"{ sha256sum " + shq(path) + " 2>/dev/null || shasum -a 256 " + shq(path) + " 2>/dev/null; };" +
			"{ stat -c %a " + shq(path) + " 2>/dev/null || stat -f %Lp " + shq(path) + " 2>/dev/null; }; true"}, nil, nil, nil)
	if err != nil {
		return "", "", false, err
	}
	lines, _ := c.linesOf(string(out))
	if len(lines) == 0 {
		return "", "", false, nil
	}
	sum, _, _ = strings.Cut(lines[0], " ")
	if len(lines) > 1 {
		mode = lines[1]
	}
	return sum, mode, true, nil
}

// linesOf is output's non-empty lines.
func (c *conn) linesOf(s string) ([]string, error) {
	var ls []string
	for l := range strings.Lines(s) {
		if l = strings.TrimSpace(l); len(l) > 0 {
			ls = append(ls, l)
		}
	}
	return ls, nil
}

// snapshot keeps a copy of the path before content with the sum (and the mode) replaces it: only when the file
// is there and the write would change it — a no-change apply leaves the snapshots alone.
func (c *conn) snapshot(ctx context.Context, path string, sum string, mode uint32) error {
	if c.history <= 0 {
		return nil
	}
	has, hasMode, exists, err := c.stateOf(ctx, path)
	if err != nil || !exists {
		return err
	}
	if has == sum && (mode == 0 || modeOf(hasMode) == mode) {
		return nil
	}
	return c.rotate(ctx, path)
}

// modeOf is stat's %a as the mode WriteFile takes.
func modeOf(s string) uint32 {
	var m uint32
	if _, err := fmt.Sscanf(s, "%o", &m); err != nil {
		return 0
	}
	return m
}

// restore puts the newest snapshot back in the path's place and shifts the rest down; a path without a snapshot
// is removed — it was added by the step being undone. It says whether a snapshot was there.
func (c *conn) restore(ctx context.Context, path string) (bool, error) {
	if c.history <= 0 {
		return false, nil
	}
	var b strings.Builder
	p, bak := shq(path), shq(path+".bak.1")
	fmt.Fprintf(&b, "if [ -f %s ]; then mv %s %s; ", bak, bak, p)
	for i := 1; i < c.history; i++ {
		fmt.Fprintf(&b, "[ ! -f %s ] || mv %s %s; ", shq(fmt.Sprintf("%s.bak.%d", path, i+1)), shq(fmt.Sprintf("%s.bak.%d", path, i+1)), shq(fmt.Sprintf("%s.bak.%d", path, i)))
	}
	b.WriteString("echo restored; else rm -f " + p + "; fi")
	out, err := c.exec(ctx, []string{"sh", "-c", b.String()}, nil, nil, nil)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "restored"), nil
}

// shaOf is the sha256 of content, as sha256sum prints it.
func shaOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// shaOfFile is the sha256 of a file of this machine.
func shaOfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
