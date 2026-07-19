package cidrtrie

import (
	"net/netip"
	"testing"
)

func TestCidrTrie(t *testing.T) {
	trie := New()

	type TestCaseInput struct {
		prefix string
		cc     string
	}

	inserts := []TestCaseInput{
		{prefix: "198.51.0.0/16", cc: "JP"},
		{prefix: "198.51.100.0/24", cc: "US"},
		{prefix: "10.0.0.0/8", cc: "PRIVATE"},
		{prefix: "10.1.0.0/16", cc: "SPECIAL"},
		{prefix: "192.168.0.0/16", cc: "LAN"},
	}
	for _, cs := range inserts {
		trie.Insert(netip.MustParsePrefix(cs.prefix), cs.cc)
	}

	type TestCaseOutput struct {
		addr string
		cc   string
	}
	queries := []TestCaseOutput{
		// 198.51.0.0/16
		{addr: "198.51.1.1", cc: "JP"},
		{addr: "198.51.255.255", cc: "JP"},

		// /24 が /16 より優先
		{addr: "198.51.100.0", cc: "US"},
		{addr: "198.51.100.1", cc: "US"},
		{addr: "198.51.100.255", cc: "US"},

		// /24 の外
		{addr: "198.51.101.1", cc: "JP"},

		// 10.0.0.0/8
		{addr: "10.2.3.4", cc: "PRIVATE"},

		// /16 が /8 より優先
		{addr: "10.1.2.3", cc: "SPECIAL"},

		// 192.168.0.0/16
		{addr: "192.168.1.1", cc: "LAN"},

		// マッチしない
		{addr: "8.8.8.8", cc: ""},
		{addr: "127.0.0.1", cc: ""},
		{addr: "172.16.0.1", cc: ""},
	}

	for _, query := range queries {
		res := trie.Search(netip.MustParseAddr(query.addr))
		t.Logf("%s: expected %s, actual %s", query.addr, query.cc, res)
		if res != query.cc {
			t.Errorf("%s: expected %s, but got %s", query.addr, query.cc, res)
		}
	}
}
