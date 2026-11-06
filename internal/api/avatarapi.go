package api

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"mime"
	"net/http"

	"github.com/MkDocsEditor/MkDocsEditor-Backend/internal/auth"
	"github.com/labstack/echo/v5"
	xdraw "golang.org/x/image/draw"
)

const (
	// FeatureAvatars means accounts have profile pictures, see docs/auth.md
	FeatureAvatars = "avatars"

	// avatarMaxUpload is the largest image an upload may have, in bytes
	avatarMaxUpload = 5 << 20
	// avatarSize is the width and height of a stored avatar
	avatarSize    = 256
	avatarQuality = 85

	urlParamAvatar = "avatar"
)

// avatarUploadTypes are the content types of avatar uploads
var avatarUploadTypes = map[string]string{"image/jpeg": "jpeg", "image/png": "png", "image/webp": "webp"}

var errInvalidImage = errors.New("the image cannot be read: it must be a JPEG, PNG or WebP of at most 50 megapixels")

// normalizeAvatar turns an uploaded image into a stored avatar: the EXIF orientation applied, the
// center square cut out, scaled to avatarSize and encoded as JPEG without metadata. Transparent
// parts become white.
func normalizeAvatar(data []byte) ([]byte, error) {
	config, format, err := decodeConfig(data)
	if err != nil || format == "gif" || config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > thumbnailMaxPixels {
		return nil, errInvalidImage
	}
	src, err := decodeImage(data, format)
	if err != nil {
		return nil, errInvalidImage
	}
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	// the center square of the turned image is the turned center square
	square := image.Rect(0, 0, side, side).Add(image.Pt(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2))
	dst := image.NewNRGBA(image.Rect(0, 0, avatarSize, avatarSize))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, square, draw.Over, nil)
	var out image.Image = dst
	if format == "jpeg" {
		out = orient(dst, jpegOrientation(data))
	}
	var buf bytes.Buffer
	if err = jpeg.Encode(&buf, out, &jpeg.Options{Quality: avatarQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readAvatarUpload checks the content type and the size of an upload and normalizes it. It
// answers the request itself if the upload is not usable.
func (rs *RestService) readAvatarUpload(c *echo.Context) ([]byte, error) {
	req := c.Request()
	mediaType, _, err := mime.ParseMediaType(req.Header.Get(echo.HeaderContentType))
	if _, ok := avatarUploadTypes[mediaType]; err != nil || !ok {
		return nil, responded(apiError(c, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			"the content type must be image/jpeg, image/png or image/webp", nil))
	}
	tooLarge := func() error {
		return responded(apiError(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "the image may have at most 5 MB",
			map[string]any{"maxBytes": avatarMaxUpload}))
	}
	if req.ContentLength > avatarMaxUpload {
		return nil, tooLarge()
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, avatarMaxUpload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > avatarMaxUpload {
		return nil, tooLarge()
	}

	select {
	case rs.thumbnailSlots <- struct{}{}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	defer func() { <-rs.thumbnailSlots }()
	image, err := normalizeAvatar(data)
	if errors.Is(err, errInvalidImage) {
		return nil, responded(apiError(c, http.StatusBadRequest, CodeInvalidImage, err.Error(), nil))
	}
	return image, err
}

// GET /avatars/{accountId}/{avatar}/
func (rs *RestService) getAvatar(c *echo.Context) error {
	hash := c.Param(urlParamAvatar)
	data, err := rs.authenticator.Avatar(c.Param(urlParamID), hash)
	if errors.Is(err, auth.ErrNotFound) {
		return apiError(c, http.StatusNotFound, CodeNotFound, "no such avatar", nil)
	}
	if err != nil {
		return rs.ReturnError(c, err)
	}
	etag := `"` + hash + `"`
	h := c.Response().Header()
	h.Set(echo.HeaderCacheControl, "private, max-age=31536000, immutable")
	h.Set("ETag", etag)
	if c.Request().Header.Get("If-None-Match") == etag {
		return c.NoContent(http.StatusNotModified)
	}
	return c.Blob(http.StatusOK, "image/jpeg", data)
}

// PUT /account/avatar/
func (rs *RestService) putAccountAvatar(c *echo.Context) error {
	image, err := rs.readAvatarUpload(c)
	if err != nil {
		return err
	}
	user := UserFromContext(c)
	u, err := rs.authenticator.SetAvatar(user.ID, image, user, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	rs.sync.AccountChanged(u.ID)
	return rs.writeAccount(c, u)
}

// DELETE /account/avatar/
func (rs *RestService) deleteAccountAvatar(c *echo.Context) error {
	user := UserFromContext(c)
	u, err := rs.authenticator.RemoveAvatar(user.ID, user, requestInfo(c))
	if err != nil {
		return rs.authError(c, err)
	}
	rs.sync.AccountChanged(u.ID)
	return rs.writeAccount(c, u)
}

// PUT /admin/users/{id}/avatar/
func (rs *RestService) adminPutAvatar(c *echo.Context) error {
	image, err := rs.readAvatarUpload(c)
	if err != nil {
		return err
	}
	return rs.writeAdminAvatar(c, func(id string) (*auth.User, error) {
		return rs.authenticator.SetAvatar(id, image, UserFromContext(c), requestInfo(c))
	})
}

// DELETE /admin/users/{id}/avatar/
func (rs *RestService) adminDeleteAvatar(c *echo.Context) error {
	return rs.writeAdminAvatar(c, func(id string) (*auth.User, error) {
		return rs.authenticator.RemoveAvatar(id, UserFromContext(c), requestInfo(c))
	})
}

func (rs *RestService) writeAdminAvatar(c *echo.Context, change func(id string) (*auth.User, error)) error {
	u, err := change(c.Param(urlParamID))
	if err != nil {
		return rs.authError(c, err)
	}
	rs.sync.AccountChanged(u.ID)
	admin, err := rs.authenticator.AdminUser(u.ID)
	if err != nil {
		return rs.authError(c, err)
	}
	return c.JSONPretty(http.StatusOK, admin, jsonIndent)
}
