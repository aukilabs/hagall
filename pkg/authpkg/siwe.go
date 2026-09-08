package authpkg

import (
	"github.com/aukilabs/go-tooling/pkg/errors"
	"github.com/ethereum/go-ethereum/common"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type SIWEMessage struct {
	Domain     string
	AddressHex common.Address
	AddressRaw string
	Statement  string
	URI        string
	Version    string
	ChainID    int64
	Nonce      string
	IssuedAt   time.Time
	Expiration *time.Time
	NotBefore  *time.Time
	RequestID  string
	Resources  []string
}

func ParseSIWEMessage(message string) (*SIWEMessage, error) {
	return parseSIWEMessageInternal(message)
}

func parseSIWEMessageInternal(message string) (*SIWEMessage, error) {
	normalized := strings.ReplaceAll(message, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	if len(lines) < 6 {
		return nil, errors.New("invalid SIWE message: insufficient lines")
	}

	const headerSuffix = " wants you to sign in with your Ethereum account:"
	if !strings.HasSuffix(lines[0], headerSuffix) {
		return nil, errors.New("invalid SIWE message: malformed header")
	}
	domain := strings.TrimSpace(strings.TrimSuffix(lines[0], headerSuffix))
	if domain == "" {
		return nil, errors.New("invalid SIWE message: missing domain")
	}

	addressLine := strings.TrimSpace(lines[1])
	if !common.IsHexAddress(addressLine) {
		return nil, errors.New("invalid SIWE message: invalid address")
	}
	address := common.HexToAddress(addressLine)

	idx := 2
	if idx >= len(lines) || lines[idx] != "" {
		return nil, errors.New("invalid SIWE message: missing empty line after address")
	}
	idx++

	var statementLines []string
	for idx < len(lines) && lines[idx] != "" {
		trimmed := strings.TrimSpace(lines[idx])
		if isFieldLine(trimmed) || trimmed == "Resources:" {
			break
		}
		statementLines = append(statementLines, lines[idx])
		idx++
	}
	if idx < len(lines) && lines[idx] == "" {
		idx++
	}

	fields := make(map[string]string)
	var resources []string

	for idx < len(lines) {
		line := lines[idx]
		idx++

		if line == "" {
			continue
		}

		if line == "Resources:" {
			for idx < len(lines) {
				resLine := lines[idx]
				if strings.HasPrefix(resLine, "- ") {
					resources = append(resources, strings.TrimSpace(strings.TrimPrefix(resLine, "- ")))
					idx++
					continue
				}
				if resLine == "" {
					idx++
					continue
				}
				idx--
				break
			}
			continue
		}

		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			return nil, errors.New("invalid SIWE message: malformed field").WithTag("line", line)
		}
		fields[parts[0]] = strings.TrimSpace(parts[1])
	}

	for _, k := range []string{"URI", "Version", "Chain ID", "Nonce", "Issued At"} {
		if _, ok := fields[k]; !ok {
			return nil, errors.New("invalid SIWE message: missing field").WithTag("field", k)
		}
	}

	uriStr := fields["URI"]
	if _, err := url.Parse(uriStr); err != nil {
		return nil, errors.New("invalid SIWE message: invalid uri").Wrap(err)
	}

	chainID, err := strconv.ParseInt(fields["Chain ID"], 10, 64)
	if err != nil {
		return nil, errors.New("invalid SIWE message: invalid chain id").Wrap(err)
	}

	issuedAt, err := parseRFC3339(fields["Issued At"])
	if err != nil {
		return nil, errors.New("invalid SIWE message: invalid issuedAt").Wrap(err)
	}

	var expirationPtr *time.Time
	if expStr, ok := fields["Expiration Time"]; ok && expStr != "" {
		exp, err := parseRFC3339(expStr)
		if err != nil {
			return nil, errors.New("invalid SIWE message: invalid expiration").Wrap(err)
		}
		expirationPtr = &exp
	}

	var notBeforePtr *time.Time
	if nbfStr, ok := fields["Not Before"]; ok && nbfStr != "" {
		nbf, err := parseRFC3339(nbfStr)
		if err != nil {
			return nil, errors.New("invalid SIWE message: invalid notBefore").Wrap(err)
		}
		notBeforePtr = &nbf
	}

	statement := strings.Join(statementLines, "\n")

	return &SIWEMessage{
		Domain:     domain,
		AddressHex: address,
		AddressRaw: addressLine,
		Statement:  statement,
		URI:        uriStr,
		Version:    fields["Version"],
		ChainID:    chainID,
		Nonce:      fields["Nonce"],
		IssuedAt:   issuedAt,
		Expiration: expirationPtr,
		NotBefore:  notBeforePtr,
		RequestID:  fields["Request ID"],
		Resources:  resources,
	}, nil
}

func parseRFC3339(value string) (time.Time, error) {
	tm, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return tm.UTC(), nil
}

func isFieldLine(line string) bool {
	fieldPrefixes := []string{
		"URI:",
		"Version:",
		"Chain ID:",
		"Nonce:",
		"Issued At:",
		"Expiration Time:",
		"Not Before:",
		"Request ID:",
	}
	for _, prefix := range fieldPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
