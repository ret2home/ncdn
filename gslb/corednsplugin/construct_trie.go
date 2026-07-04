// https://github.com/ipverse/country-ip-blocks/

package corednsplugin

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"

	cidrtrie "github.com/yzp0n/ncdn/gslb/cidr_trie"
)

type CidrJson struct {
	CountryCode string `json:"countryCode"`
	Prefixes    struct {
		Ipv4 []string `json:"ipv4"`
	} `json:"prefixes"`
}

func ConstructTrieFromDB(root string) (*cidrtrie.CidrTrie, error) {
	trie := cidrtrie.New()
	files, err := filepath.Glob(filepath.Join(root, "*", "aggregated.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		cc := filepath.Base(filepath.Dir(path))
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		var data CidrJson
		if err := json.NewDecoder(f).Decode(&data); err != nil {
			f.Close()
			return nil, err
		}
		f.Close()
		for _, s := range data.Prefixes.Ipv4 {
			trie.Insert(netip.MustParsePrefix(s), cc)
		}
	}
	return trie, nil
}
