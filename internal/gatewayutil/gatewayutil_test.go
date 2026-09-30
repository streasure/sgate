package gatewayutil

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGenerateConnectionID(t *testing.T) {
	id1 := GenerateConnectionID()
	id2 := GenerateConnectionID()
	if id1 == "" || id2 == "" {
		t.Error("connection ID should not be empty")
	}
	if id1 == id2 {
		t.Error("connection IDs should be unique")
	}
}

func TestGenerateConnectionIDHasProcessPrefix(t *testing.T) {
	if len(connIDPrefix) != 12 {
		t.Errorf("process prefix %q should be 12 hex chars", connIDPrefix)
	}
	if _, err := strconv.ParseUint(connIDPrefix, 16, 64); err != nil {
		t.Errorf("process prefix %q is not hex: %v", connIDPrefix, err)
	}

	id := GenerateConnectionID()
	if !strings.Contains(id, connIDPrefix) {
		t.Errorf("connection ID %q should embed the process prefix %q", id, connIDPrefix)
	}
}

func TestFormatConnectionIDDoesNotWrap(t *testing.T) {
	prefix := "fffffffffffe"
	below := formatConnectionID(1700000000000, prefix, 999999)
	above := formatConnectionID(1700000000000, prefix, 1000000)
	if below == above {
		t.Error("counter must not wrap at 1e6")
	}
	if !strings.HasSuffix(below, "0000999999") {
		t.Errorf("unexpected id %q", below)
	}
	if !strings.HasSuffix(above, "0001000000") {
		t.Errorf("unexpected id %q", above)
	}

	max := formatConnectionID(1700000000000, prefix, ^uint64(0))
	if max == formatConnectionID(1700000000000, prefix, 0) {
		t.Error("max counter must not collide with zero counter")
	}
}

func TestFormatConnectionIDProcessScoped(t *testing.T) {
	one := formatConnectionID(1700000000000, "aaaaaaaaaaaa", 1)
	two := formatConnectionID(1700000000000, "bbbbbbbbbbbb", 1)
	if one == two {
		t.Error("ids from different processes with the same counter must differ")
	}
}

func TestGenerateConnectionIDConcurrency(t *testing.T) {
	ids := sync.Map{}
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			id := GenerateConnectionID()
			if _, loaded := ids.LoadOrStore(id, struct{}{}); loaded {
				t.Errorf("duplicate connection ID: %s", id)
			}
		})
	}
	wg.Wait()
}

func TestGetString(t *testing.T) {
	m := map[string]any{
		"name": "test",
		"num":  123,
	}
	if GetString(m, "name") != "test" {
		t.Error("GetString should return string value")
	}
	if GetString(m, "missing") != "" {
		t.Error("GetString should return empty for missing key")
	}
	if GetString(m, "num") != "" {
		t.Error("GetString should return empty for non-string value")
	}
}

func TestGetInt(t *testing.T) {
	m := map[string]any{
		"int_val":   42,
		"float_val": 3.14,
		"str_val":   "hello",
	}
	if GetInt(m, "int_val") != 42 {
		t.Error("GetInt should return int value")
	}
	if GetInt(m, "float_val") != 3 {
		t.Error("GetInt should convert float64 to int")
	}
	if GetInt(m, "str_val") != 0 {
		t.Error("GetInt should return 0 for non-numeric value")
	}
	if GetInt(m, "missing") != 0 {
		t.Error("GetInt should return 0 for missing key")
	}
}

func TestGetFloat(t *testing.T) {
	m := map[string]any{
		"float_val": 3.14,
		"int_val":   42,
		"str_val":   "hello",
	}
	if GetFloat(m, "float_val") != 3.14 {
		t.Error("GetFloat should return float64 value")
	}
	if GetFloat(m, "int_val") != 42.0 {
		t.Error("GetFloat should convert int to float64")
	}
	if GetFloat(m, "str_val") != 0 {
		t.Error("GetFloat should return 0 for non-numeric value")
	}
	if GetFloat(m, "missing") != 0 {
		t.Error("GetFloat should return 0 for missing key")
	}
}

func TestParseDurationDefault(t *testing.T) {
	def := 5 * time.Second
	if ParseDurationDefault("", def) != def {
		t.Error("empty string should return default")
	}
	if ParseDurationDefault("3s", def) != 3*time.Second {
		t.Error("valid duration should be parsed")
	}
	if ParseDurationDefault("invalid", def) != def {
		t.Error("invalid duration should return default")
	}
}

func TestSimpleHash(t *testing.T) {
	h1 := SimpleHash("hello")
	h2 := SimpleHash("hello")
	h3 := SimpleHash("world")
	if h1 != h2 {
		t.Error("same input should produce same hash")
	}
	if h1 == h3 {
		t.Error("different input should produce different hash")
	}
}

func TestCopyMap(t *testing.T) {
	orig := map[string]string{"a": "1", "b": "2"}
	cp := CopyMap(orig)
	if len(cp) != len(orig) {
		t.Error("copy should have same length")
	}
	cp["c"] = "3"
	if _, ok := orig["c"]; ok {
		t.Error("modifying copy should not affect original")
	}
}

func TestMaxInt(t *testing.T) {
	if MaxInt(3, 5) != 5 {
		t.Error("MaxInt(3,5) should be 5")
	}
	if MaxInt(5, 3) != 5 {
		t.Error("MaxInt(5,3) should be 5")
	}
	if MaxInt(3, 3) != 3 {
		t.Error("MaxInt(3,3) should be 3")
	}
}
