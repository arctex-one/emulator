package sim

import (
	"math/big"
	"regexp"
	"strings"
)

var decimalPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)

func contractAmount(v string) (string, error) {
	if !decimalPattern.MatchString(v) {
		return "", problem(400, "INVALID_REQUEST", "A positive decimal string is required")
	}
	d := amount(v)
	if !d.IsPositive() {
		return "", problem(400, "INVALID_REQUEST", "A positive decimal string is required")
	}
	return d.String(), nil
}
func base58(input []byte) string {
	n := new(big.Int).SetBytes(input)
	base := big.NewInt(58)
	rem := new(big.Int)
	out := []byte{}
	for n.Sign() > 0 {
		n.QuoRem(n, base, rem)
		out = append(out, "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"[rem.Int64()])
	}
	for _, b := range input {
		if b != 0 {
			break
		}
		out = append(out, '1')
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func decodeBase58(value string) []byte {
	n := new(big.Int)
	base := big.NewInt(58)
	for _, character := range value {
		digit := strings.IndexRune("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz", character)
		if digit < 0 {
			return nil
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(digit)))
	}
	out := n.Bytes()
	for _, character := range value {
		if character != '1' {
			break
		}
		out = append([]byte{0}, out...)
	}
	return out
}
