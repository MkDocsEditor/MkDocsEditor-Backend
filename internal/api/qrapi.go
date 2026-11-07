package api

import (
	"log"
	"net/http"
	"sort"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/activity"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/references"
	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/wiki"
	"github.com/labstack/echo/v5"
)

const (
	// FeatureQRCodes means pages can carry QR code keys, see docs/qr.md
	FeatureQRCodes = "qr-codes"

	CodeQRConflict = "qr-conflict"
	CodeQRGone     = "qr-gone"

	urlParamKey = "key"
)

type (
	// QRItem is a document with a QR code
	QRItem struct {
		Item  *references.ItemRef `json:"item"`
		Title string              `json:"title"`
	}

	// QRCodeResponse is a key on exactly one document
	QRCodeResponse struct {
		Key   string  `json:"key"`
		Label *string `json:"label"`
		QRItem
		LastScannedAt *string `json:"lastScannedAt"`
	}

	// QRConflictResponse is a key on more than one document
	QRConflictResponse struct {
		Key           string   `json:"key"`
		Items         []QRItem `json:"items"`
		LastScannedAt *string  `json:"lastScannedAt"`
	}

	// QRInvalidResponse is a qr value that is no valid key
	QRInvalidResponse struct {
		Value string `json:"value"`
		QRItem
	}

	// QRGoneResponse is a key no document has any more
	QRGoneResponse struct {
		Key           string  `json:"key"`
		Label         *string `json:"label"`
		LastPath      string  `json:"lastPath"`
		LastScannedAt *string `json:"lastScannedAt"`
	}

	QRListResponse struct {
		Codes     []QRCodeResponse     `json:"codes"`
		Conflicts []QRConflictResponse `json:"conflicts"`
		Invalid   []QRInvalidResponse  `json:"invalid"`
		Gone      []QRGoneResponse     `json:"gone"`
	}
)

func qrItem(d *wiki.Document) QRItem {
	return QRItem{Item: references.DocumentRef(d), Title: d.Title}
}

func optionalTime(store map[string]activity.QRCode, key string) *string {
	if c, ok := store[key]; ok && !c.LastScannedAt.IsZero() {
		t := formatTime(c.LastScannedAt)
		return &t
	}
	return nil
}

// watchQRCodes keeps the remembered keys of activity.db current, so a key whose page is gone
// answers 410 with its last page
func (rs *RestService) watchQRCodes() {
	save := func(oldTree, newTree *wiki.Section) {
		before, _ := wiki.QRCodes(oldTree)
		after, _ := wiki.QRCodes(newTree)
		var changed []activity.QRCode
		for key, list := range after {
			p := list[0]
			if old, ok := before[key]; ok && old[0].Document.RelPath == p.Document.RelPath && sameLabel(old[0].Code.Label, p.Code.Label) {
				continue
			}
			changed = append(changed, activity.QRCode{Key: key, LastPath: p.Document.RelPath, Label: p.Code.Label})
		}
		rs.sync.Activity().SaveQRCodes(changed)
	}
	rs.treeManager.AddTreeChangeListener(save)
	// the tree of the start was built before the listener
	save(nil, rs.treeManager.Root())
}

func sameLabel(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// GET /qr/
func (rs *RestService) getQRCodes(c *echo.Context) error {
	byKey, invalid := wiki.QRCodes(rs.treeManager.Root())
	store, err := rs.sync.Activity().QRCodes()
	if err != nil {
		return rs.ReturnError(c, err)
	}
	out := &QRListResponse{Codes: []QRCodeResponse{}, Conflicts: []QRConflictResponse{}, Invalid: []QRInvalidResponse{}, Gone: []QRGoneResponse{}}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		list := byKey[key]
		if len(list) == 1 {
			out.Codes = append(out.Codes, QRCodeResponse{Key: key, Label: list[0].Code.Label, QRItem: qrItem(list[0].Document),
				LastScannedAt: optionalTime(store, key)})
			continue
		}
		out.Conflicts = append(out.Conflicts, qrConflict(key, list, store))
	}
	for _, i := range invalid {
		out.Invalid = append(out.Invalid, QRInvalidResponse{Value: i.Value, QRItem: qrItem(i.Document)})
	}
	for key, code := range store {
		if _, ok := byKey[key]; !ok {
			out.Gone = append(out.Gone, qrGone(code))
		}
	}
	sort.Slice(out.Gone, func(i, j int) bool { return out.Gone[i].Key < out.Gone[j].Key })
	return c.JSONPretty(http.StatusOK, out, jsonIndent)
}

func qrConflict(key string, list []wiki.QRPlacement, store map[string]activity.QRCode) QRConflictResponse {
	r := QRConflictResponse{Key: key, Items: make([]QRItem, 0, len(list)), LastScannedAt: optionalTime(store, key)}
	for _, p := range list {
		r.Items = append(r.Items, qrItem(p.Document))
	}
	return r
}

func qrGone(code activity.QRCode) QRGoneResponse {
	return QRGoneResponse{Key: code.Key, Label: code.Label, LastPath: code.LastPath,
		LastScannedAt: optionalTime(map[string]activity.QRCode{code.Key: code}, code.Key)}
}

// GET /qr/{key}/, with ?scan=1 for a scan of the sticker
func (rs *RestService) getQRCode(c *echo.Context) error {
	key, ok := wiki.NormalizeQRKey(c.Param(urlParamKey))
	if !ok {
		return apiError(c, http.StatusNotFound, CodeNotFound, "no page has this QR code", nil)
	}
	byKey, _ := wiki.QRCodes(rs.treeManager.Root())
	list := byKey[key]
	store := rs.sync.Activity()
	if c.QueryParam("scan") == "1" {
		if _, err := store.RecordQRScan(key); err != nil {
			log.Printf("Unable to record the scan of the QR code %s: %v", key, err)
		}
	}
	code, err := store.QRCode(key)
	if err != nil {
		return rs.ReturnError(c, err)
	}
	remembered := map[string]activity.QRCode{}
	if code != nil {
		remembered[key] = *code
	}
	switch {
	case len(list) == 1:
		return c.JSONPretty(http.StatusOK, &QRCodeResponse{Key: key, Label: list[0].Code.Label, QRItem: qrItem(list[0].Document),
			LastScannedAt: optionalTime(remembered, key)}, jsonIndent)
	case len(list) > 1:
		conflict := qrConflict(key, list, remembered)
		return apiError(c, http.StatusConflict, CodeQRConflict, "more than one page has this QR code",
			map[string]any{"key": key, "items": conflict.Items, "lastScannedAt": conflict.LastScannedAt})
	case code != nil:
		gone := qrGone(*code)
		return apiError(c, http.StatusGone, CodeQRGone, "no page has this QR code any more",
			map[string]any{"key": key, "label": gone.Label, "lastPath": gone.LastPath, "lastScannedAt": gone.LastScannedAt})
	}
	return apiError(c, http.StatusNotFound, CodeNotFound, "no page has this QR code", nil)
}
