package symbols

import (
	"debug/elf"
	"testing"
)

func tableOf(syms ...elf.Symbol) *Table {
	return &Table{syms: syms}
}

func TestResolveFindsTheContainingSymbol(t *testing.T) {
	table := tableOf(
		elf.Symbol{Name: "first", Value: 0x100, Size: 0x50},
		elf.Symbol{Name: "second", Value: 0x200, Size: 0x30},
	)

	name, delta, ok := table.Resolve(0x210)
	if !ok || name != "second" || delta != 0x10 {
		t.Fatalf("Resolve(0x210) = %q, %#x, %v; want second, 0x10, true", name, delta, ok)
	}
}

func TestResolveReturnsFalseBeforeTheFirstSymbol(t *testing.T) {
	table := tableOf(elf.Symbol{Name: "only", Value: 0x100, Size: 0x50})

	if _, _, ok := table.Resolve(0x50); ok {
		t.Fatalf("Resolve before the first symbol's address should not match")
	}
}

func TestResolveReturnsFalsePastASymbolsSize(t *testing.T) {
	table := tableOf(elf.Symbol{Name: "only", Value: 0x100, Size: 0x50})

	if _, _, ok := table.Resolve(0x100 + 0x50); ok {
		t.Fatalf("Resolve at a symbol's end address (exclusive) should not match")
	}
}

func TestResolveAcceptsAZeroSizeSymbolAsOpenEnded(t *testing.T) {
	// Some symbols (common in stripped-down tables) carry no size at all;
	// a later address should still resolve to the nearest preceding one.
	table := tableOf(elf.Symbol{Name: "only", Value: 0x100, Size: 0})

	name, delta, ok := table.Resolve(0x100 + 0x1000)
	if !ok || name != "only" || delta != 0x1000 {
		t.Fatalf("Resolve(zero-size symbol + offset) = %q, %#x, %v; want only, 0x1000, true", name, delta, ok)
	}
}

func TestResolveOnAnEmptyTable(t *testing.T) {
	table := tableOf()

	if _, _, ok := table.Resolve(0x1000); ok {
		t.Fatalf("Resolve on an empty table should never match")
	}
}
