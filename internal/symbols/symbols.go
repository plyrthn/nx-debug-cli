// Package symbols resolves a runtime address to a symbol name using an
// unstripped ELF built alongside the target's own stripped module - for
// anything built with this SDK's toolchain, that's the ".nss" file this
// project's crash dump reader and gdb stub client both already recognise
// as a module's own build-time name (see internal/nxdmp and
// internal/htc/gdbstub.go's Modules).
package symbols

import (
	"debug/elf"
	"fmt"
	"sort"
)

// Table is a module's function symbols, ready to resolve an offset within
// the module against the nearest symbol at or before it.
type Table struct {
	syms []elf.Symbol // sorted by Value
}

// Load reads an ELF's function symbols. The file is read whole and closed
// before returning; nothing about it is kept open.
func Load(path string) (*Table, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("symbols: open %s: %w", path, err)
	}
	defer f.Close()

	all, err := f.Symbols()
	if err != nil {
		return nil, fmt.Errorf("symbols: read symbol table in %s: %w", path, err)
	}
	t := &Table{}
	for _, s := range all {
		if elf.ST_TYPE(s.Info) != elf.STT_FUNC || s.Name == "" {
			continue
		}
		t.syms = append(t.syms, s)
	}
	sort.Slice(t.syms, func(i, j int) bool { return t.syms[i].Value < t.syms[j].Value })
	return t, nil
}

// Resolve finds the function symbol containing off, an address already
// relative to the module's own load base (a live PC minus the base
// internal/htc/gdbstub.go's Modules reports for it). Returns false if off
// falls before the first symbol or past the one it's nearest to.
func (t *Table) Resolve(off uint64) (name string, delta uint64, ok bool) {
	if len(t.syms) == 0 {
		return "", 0, false
	}
	i := sort.Search(len(t.syms), func(i int) bool { return t.syms[i].Value > off }) - 1
	if i < 0 {
		return "", 0, false
	}
	sym := t.syms[i]
	if sym.Size != 0 && off >= sym.Value+sym.Size {
		return "", 0, false
	}
	return sym.Name, off - sym.Value, true
}
