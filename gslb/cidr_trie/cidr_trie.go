package cidrtrie

import (
	"encoding/binary"
	"net/netip"
)

type CidrTrie struct {
	Childs      [2]*CidrTrie
	CountryCode string
}

func New() *CidrTrie {
	c := CidrTrie{Childs: [2]*CidrTrie{nil, nil}, CountryCode: ""}
	return &c
}
func InsertInternal(t *CidrTrie, addrnum uint32, prefix_length int, depth int, cc string) {
	if depth == prefix_length {
		t.CountryCode = cc
		return
	}
	bit := (addrnum >> (31 - depth)) & 1

	if t.Childs[bit] == nil {
		t.Childs[bit] = New()
	}
	InsertInternal(t.Childs[bit], addrnum, prefix_length, depth+1, cc)
}

func (t *CidrTrie) Insert(prefix netip.Prefix, cc string) {
	ip4 := prefix.Addr().As4()
	ipInt := binary.BigEndian.Uint32(ip4[:])

	InsertInternal(t, ipInt, prefix.Bits(), 0, cc)
}

func SearchInternal(t *CidrTrie, addrnum uint32, depth int) string {
	if depth == 32 {
		return t.CountryCode
	}

	bit := (addrnum >> (31 - depth)) & 1

	if t.Childs[bit] == nil {
		return t.CountryCode
	}
	cc := SearchInternal(t.Childs[bit], addrnum, depth+1)
	if cc != "" {
		return cc
	}
	return t.CountryCode
}
func (t *CidrTrie) Search(ip netip.Addr) string {
	ip4 := ip.As4()
	ipInt := binary.BigEndian.Uint32(ip4[:])
	return SearchInternal(t, ipInt, 0)
}
