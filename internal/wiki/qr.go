package wiki

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// qrKeyPattern is a valid QR code key after lowercasing, see docs/qr.md
var qrKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// QRCode is a QR code key of a page with its optional label
type QRCode struct {
	Key   string  `json:"key"`
	Label *string `json:"label"`
}

// NormalizeQRKey returns the key in its compared form and whether it is valid
func NormalizeQRKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	return key, qrKeyPattern.MatchString(key)
}

// parseQRCodes reads the qr front matter value: a key, a {key, label} map, or a list of both.
// Invalid entries are returned as written. A key given twice counts once.
func parseQRCodes(v any) ([]QRCode, []string) {
	codes := []QRCode{}
	var invalid []string
	seen := map[string]bool{}
	add := func(e any) {
		var key string
		var label *string
		switch e := e.(type) {
		case nil:
			return
		case map[string]any:
			k, ok := e["key"]
			if !ok || k == nil {
				invalid = append(invalid, fmt.Sprint(e))
				return
			}
			key = fmt.Sprint(k)
			if l, ok := e["label"]; ok && l != nil {
				s := fmt.Sprint(l)
				label = &s
			}
		case []any:
			invalid = append(invalid, fmt.Sprint(e))
			return
		default:
			key = fmt.Sprint(e)
		}
		normalized, ok := NormalizeQRKey(key)
		if !ok {
			invalid = append(invalid, key)
			return
		}
		if !seen[normalized] {
			seen[normalized] = true
			codes = append(codes, QRCode{Key: normalized, Label: label})
		}
	}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			add(e)
		}
	} else {
		add(v)
	}
	return codes, invalid
}

// QRPlacement is a QR code on a document
type QRPlacement struct {
	Code     QRCode
	Document *Document
}

// QRInvalid is a qr value of a document that is no valid key
type QRInvalid struct {
	Value    string
	Document *Document
}

// QRCodes collects the QR codes of a tree by key, each list sorted by document path, and the
// invalid values sorted by document path
func QRCodes(root *Section) (map[string][]QRPlacement, []QRInvalid) {
	byKey := map[string][]QRPlacement{}
	var invalid []QRInvalid
	for s := range root.Walk() {
		for _, d := range s.Documents {
			for _, c := range d.Meta.QR {
				byKey[c.Key] = append(byKey[c.Key], QRPlacement{Code: c, Document: d})
			}
			for _, v := range d.Meta.QRInvalid {
				invalid = append(invalid, QRInvalid{Value: v, Document: d})
			}
		}
	}
	for _, list := range byKey {
		sort.Slice(list, func(i, j int) bool { return list[i].Document.RelPath < list[j].Document.RelPath })
	}
	sort.SliceStable(invalid, func(i, j int) bool { return invalid[i].Document.RelPath < invalid[j].Document.RelPath })
	return byKey, invalid
}
