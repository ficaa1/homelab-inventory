package plugin

import (
	"errors"
	"mime"
	"strings"
)

const MaxImageBytes = 4 << 20

func validateImage(ct string, body []byte) (string, []byte, error) {
	media, _, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(media, "image/") || len(media) == len("image/") {
		return "", nil, errors.New("plugin returned a non-image content type")
	}
	if len(body) > MaxImageBytes {
		return "", nil, errors.New("plugin image exceeds 4 MiB")
	}
	return media, body, nil
}
