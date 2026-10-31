package cloud

import (
	"context"
	"net/http"
	"strings"
	"time"

	"briard.io/shared/api"
)

// SendWish tells the cloud at base about a household's wish (api.Wish): one form POST, no
// token. A surface of its own, like Casa beside the managed channel: an unmanaged node sends
// it, and it carries nothing a managed report would.
func SendWish(ctx context.Context, base string, w api.Wish) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+api.WishPath, strings.NewReader(w.Form().Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return failure("wish", resp)
	}
	return nil
}
