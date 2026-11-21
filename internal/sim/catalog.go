package sim

import (
	"bytes"
	"crypto/sha256"
	wire "github.com/arctex-one/client-go/generated"
	"strings"
)

func catalog() wire.AssetList {
	token := func(network, currency, contract string) wire.Asset {
		return wire.Asset{Network: network, Currency: currency, Kind: "TOKEN_CONTRACT", ContractAddress: ptr(contract)}
	}
	return wire.AssetList{CatalogRevision: "emulator-1", Items: []wire.AssetCatalogEntry{
		{Asset: wire.Asset{Network: "tron_mainnet", Currency: "TRX", Kind: "NATIVE"}, AccountingScale: 6, DisplayScale: 6},
		{Asset: token("tron_mainnet", "USDT", "TR7NHqjeKQxGTCi8q8ZY4pL8otgjLj6t"), AccountingScale: 6, DisplayScale: 6},
		{Asset: wire.Asset{Network: "monad_testnet", Currency: "MON", Kind: "NATIVE"}, AccountingScale: 18, DisplayScale: 8},
		{Asset: token("monad_testnet", "USDC", "0x1111111111111111111111111111111111111111"), AccountingScale: 6, DisplayScale: 6},
		{Asset: wire.Asset{Network: "arbitrum_sepolia", Currency: "ETH", Kind: "NATIVE"}, AccountingScale: 18, DisplayScale: 8},
		{Asset: token("arbitrum_sepolia", "USDC", "0x2222222222222222222222222222222222222222"), AccountingScale: 6, DisplayScale: 6},
	}}
}
func resolve(a wire.Asset) (wire.Asset, int32, error) {
	for _, i := range catalog().Items {
		if a.Network == i.Asset.Network && a.Currency == i.Asset.Currency {
			if a.Kind != i.Asset.Kind || a.ContractAddress != nil && (i.Asset.ContractAddress == nil || *a.ContractAddress != *i.Asset.ContractAddress) {
				return a, 0, problem(422, "INVALID_INPUT", "Asset identity differs from catalog")
			}
			return i.Asset, int32(i.AccountingScale), nil
		}
	}
	return a, 0, problem(422, "INVALID_INPUT", "Asset is not enabled")
}
func bridgeRoutes() wire.BridgeRouteList {
	c := catalog()
	return wire.BridgeRouteList{Items: []wire.BridgeRoute{{RouteRef: "emu_usdc_monad_arbitrum", SourceAsset: c.Items[3].Asset, DestinationAsset: c.Items[5].Asset, Modes: []string{"STANDARD", "FAST"}}}}
}
func sameAsset(a, b wire.Asset) bool { return string(raw(a)) == string(raw(b)) }
func validAddress(a wire.Asset, address string) bool {
	if strings.HasPrefix(a.Network, "tron_") {
		if len(address) != 34 || !strings.HasPrefix(address, "T") {
			return false
		}
		decoded := decodeBase58(address)
		if len(decoded) != 25 || decoded[0] != 0x41 {
			return false
		}
		first := sha256.Sum256(decoded[:21])
		second := sha256.Sum256(first[:])
		return bytes.Equal(decoded[21:], second[:4])
	}
	if len(address) != 42 || !strings.HasPrefix(address, "0x") {
		return false
	}
	return strings.IndexFunc(address[2:], func(r rune) bool { return !strings.ContainsRune("0123456789abcdefABCDEF", r) }) < 0
}
