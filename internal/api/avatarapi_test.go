package api

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"testing"
)

// stripes returns an image of w x h with vertical stripes of the colours, from left to right
func stripes(w, h int, colors ...color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		c := colors[x*len(colors)/w]
		for y := range h {
			img.Set(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	red   = color.NRGBA{R: 255, A: 255}
	green = color.NRGBA{G: 255, A: 255}
	blue  = color.NRGBA{B: 255, A: 255}
)

// near reports whether a pixel is close to a colour, JPEG is not exact
func near(c color.Color, want color.NRGBA) bool {
	r, g, b, _ := c.RGBA()
	d := func(a uint32, b uint8) int { return int(a>>8) - int(b) }
	for _, x := range []int{d(r, want.R), d(g, want.G), d(b, want.B)} {
		if x < -40 || x > 40 {
			return false
		}
	}
	return true
}

func (c *client) account() *AccountResponse {
	c.env.t.Helper()
	r := c.do(http.MethodGet, "/account/", nil)
	expect(c.env.t, r, http.StatusOK, "")
	var a AccountResponse
	r.json(c.env.t, &a)
	return &a
}

// getAvatarImage loads an avatar and decodes it
func (c *client) getAvatarImage(accountID string, avatar string) image.Image {
	c.env.t.Helper()
	r := c.do(http.MethodGet, "/avatars/"+accountID+"/"+avatar+"/", nil)
	expect(c.env.t, r, http.StatusOK, "")
	if ct := r.header.Get("Content-Type"); ct != "image/jpeg" {
		c.env.t.Fatalf("content type = %q", ct)
	}
	img, err := jpeg.Decode(bytes.NewReader(r.body))
	if err != nil {
		c.env.t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != avatarSize || b.Dy() != avatarSize {
		c.env.t.Fatalf("avatar is %v", b)
	}
	return img
}

func TestAvatarUploadServeAndRemove(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	me := admin.account()
	if me.Avatar != nil {
		t.Fatalf("new account has the avatar %q", *me.Avatar)
	}

	// red | green | blue, 600x300: the center square is all green
	r := admin.doRaw(http.MethodPut, "/account/avatar/", encodePNG(t, stripes(600, 300, red, green, green, blue)), "image/png")
	expect(t, r, http.StatusOK, "")
	var a AccountResponse
	r.json(t, &a)
	if a.Avatar == nil || len(*a.Avatar) != 16 || a.ID != me.ID {
		t.Fatalf("account after upload = %s", r.body)
	}
	first := *a.Avatar
	if got := admin.account().Avatar; got == nil || *got != first {
		t.Fatalf("GET /account/ avatar = %v", got)
	}
	img := admin.getAvatarImage(me.ID, first)
	for _, p := range []image.Point{{2, 2}, {128, 128}, {253, 253}} {
		if !near(img.At(p.X, p.Y), green) {
			t.Errorf("pixel %v = %v, want the green center", p, img.At(p.X, p.Y))
		}
	}

	r = admin.do(http.MethodGet, "/avatars/"+me.ID+"/"+first+"/", nil)
	if cc := r.header.Get("Cache-Control"); cc != "private, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", cc)
	}
	etag := r.header.Get("ETag")
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+APIPrefix+"/avatars/"+me.ID+"/"+first+"/", nil)
	req.Header.Set("If-None-Match", etag)
	req.Header.Set("Origin", testWebOrigin)
	req.AddCookie(admin.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified || resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Errorf("If-None-Match: status %d, headers %v", resp.StatusCode, resp.Header)
	}

	// a JPEG turned by EXIF: red on the left becomes red on top
	upright := encodeJPEG(t, stripes(300, 200, red, red, green, green))
	r = admin.doRaw(http.MethodPut, "/account/avatar/", withJPEGOrientation(t, upright, 6), "image/jpeg")
	expect(t, r, http.StatusOK, "")
	r.json(t, &a)
	second := *a.Avatar
	if second == first {
		t.Fatal("a new image kept the avatar version")
	}
	img = admin.getAvatarImage(me.ID, second)
	if !near(img.At(128, 20), red) || !near(img.At(128, 236), green) {
		t.Errorf("EXIF orientation not applied: top %v, bottom %v", img.At(128, 20), img.At(128, 236))
	}
	expect(t, admin.do(http.MethodGet, "/avatars/"+me.ID+"/"+first+"/", nil), http.StatusNotFound, CodeNotFound)

	// the same image again keeps the version
	r = admin.doRaw(http.MethodPut, "/account/avatar/", withJPEGOrientation(t, upright, 6), "image/jpeg")
	expect(t, r, http.StatusOK, "")
	r.json(t, &a)
	if *a.Avatar != second {
		t.Errorf("the same image changed the version to %s", *a.Avatar)
	}

	// transparent parts become white
	transparent := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	r = admin.doRaw(http.MethodPut, "/account/avatar/", encodePNG(t, transparent), "image/png")
	expect(t, r, http.StatusOK, "")
	r.json(t, &a)
	if img = admin.getAvatarImage(me.ID, *a.Avatar); !near(img.At(100, 100), color.NRGBA{R: 255, G: 255, B: 255, A: 255}) {
		t.Errorf("transparent pixel = %v, want white", img.At(100, 100))
	}
	third := *a.Avatar

	// remove, twice
	for range 2 {
		r = admin.do(http.MethodDelete, "/account/avatar/", nil)
		expect(t, r, http.StatusOK, "")
		r.json(t, &a)
		if a.Avatar != nil {
			t.Fatalf("avatar after DELETE = %s", r.body)
		}
	}
	expect(t, admin.do(http.MethodGet, "/avatars/"+me.ID+"/"+third+"/", nil), http.StatusNotFound, CodeNotFound)
	expect(t, admin.do(http.MethodGet, "/avatars/nobody/"+third+"/", nil), http.StatusNotFound, CodeNotFound)
}

func TestAvatarUploadErrors(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	valid := encodePNG(t, stripes(10, 10, red))

	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", valid, "text/plain"), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", valid, "image/gif"), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", valid, ""), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", bytes.Repeat([]byte{0}, avatarMaxUpload+1), "image/png"),
		http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", []byte("not an image"), "image/png"), http.StatusBadRequest, CodeInvalidImage)
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", valid[:len(valid)/2], "image/png"), http.StatusBadRequest, CodeInvalidImage)

	var gifData bytes.Buffer
	if err := gif.Encode(&gifData, stripes(10, 10, red), nil); err != nil {
		t.Fatal(err)
	}
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", gifData.Bytes(), "image/png"), http.StatusBadRequest, CodeInvalidImage)

	// a header that claims 100000 x 100000 pixels is refused before decoding
	bomb := encodePNG(t, stripes(1, 1, red))
	bomb[16], bomb[17], bomb[18], bomb[19] = 0, 1, 0x86, 0xa0
	bomb[20], bomb[21], bomb[22], bomb[23] = 0, 1, 0x86, 0xa0
	expect(t, admin.doRaw(http.MethodPut, "/account/avatar/", bomb, "image/png"), http.StatusBadRequest, CodeInvalidImage)

	if admin.account().Avatar != nil {
		t.Error("a failed upload set an avatar")
	}
}

func TestAvatarAdminAndOtherAccounts(t *testing.T) {
	e := newAccountEnv(t)
	admin := e.setupAdmin()
	reader := e.invite(admin, "reader", "rita", false)
	rita := reader.account()

	// readers may set their own avatar
	r := reader.doRaw(http.MethodPut, "/account/avatar/", encodePNG(t, stripes(20, 20, blue)), "image/png")
	expect(t, r, http.StatusOK, "")
	var a AccountResponse
	r.json(t, &a)
	own := *a.Avatar

	// everyone signed in sees it, nobody else may change it
	admin.getAvatarImage(rita.ID, own)
	expect(t, e.app().do(http.MethodGet, "/avatars/"+rita.ID+"/"+own+"/", nil), http.StatusUnauthorized, "")
	expect(t, reader.doRaw(http.MethodPut, "/admin/users/"+admin.account().ID+"/avatar/", encodePNG(t, stripes(20, 20, red)), "image/png"),
		http.StatusForbidden, CodeForbidden)

	// the admin list has it, an admin can replace and remove it
	r = admin.do(http.MethodGet, "/admin/users/", nil)
	expect(t, r, http.StatusOK, "")
	if !bytes.Contains(r.body, []byte(`"avatar": "`+own+`"`)) {
		t.Errorf("admin users = %s", r.body)
	}
	r = admin.doRaw(http.MethodPut, "/admin/users/"+rita.ID+"/avatar/", encodePNG(t, stripes(20, 20, red)), "image/png")
	expect(t, r, http.StatusOK, "")
	var u struct {
		ID     string  `json:"id"`
		Avatar *string `json:"avatar"`
		Status string  `json:"status"`
	}
	r.json(t, &u)
	if u.ID != rita.ID || u.Avatar == nil || *u.Avatar == own || u.Status != "active" {
		t.Fatalf("admin avatar upload = %s", r.body)
	}
	if got := reader.account().Avatar; got == nil || *got != *u.Avatar {
		t.Errorf("the account sees %v", got)
	}
	r = admin.do(http.MethodDelete, "/admin/users/"+rita.ID+"/avatar/", nil)
	expect(t, r, http.StatusOK, "")
	r.json(t, &u)
	if u.Avatar != nil {
		t.Errorf("after the admin removed it: %s", r.body)
	}
	expect(t, admin.do(http.MethodPut, "/admin/users/nobody/avatar/", nil), http.StatusUnsupportedMediaType, CodeUnsupportedMediaType)
	expect(t, admin.doRaw(http.MethodPut, "/admin/users/nobody/avatar/", encodePNG(t, stripes(20, 20, red)), "image/png"), http.StatusNotFound, "")

	// a deleted account loses its avatar
	r = reader.doRaw(http.MethodPut, "/account/avatar/", encodePNG(t, stripes(20, 20, green)), "image/png")
	expect(t, r, http.StatusOK, "")
	r.json(t, &a)
	expect(t, admin.do(http.MethodDelete, "/admin/users/"+rita.ID+"/", nil), http.StatusNoContent, "")
	expect(t, admin.do(http.MethodGet, "/avatars/"+rita.ID+"/"+*a.Avatar+"/", nil), http.StatusNotFound, CodeNotFound)

	// the feature is announced
	r = e.app().do(http.MethodGet, "/server-info/", nil)
	if !bytes.Contains(r.body, []byte(`"`+FeatureAvatars+`"`)) {
		t.Errorf("server info = %s", r.body)
	}
}
