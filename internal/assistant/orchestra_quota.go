package assistant

import (
	"context"
	"net/http"
	"time"
)

// DesktopFiveHourRemaining reads only existing, valid Desktop credentials and
// uses the existing quota query; it never refreshes or writes credentials.
func (r Reader) DesktopFiveHourRemaining(ctx context.Context) (float64, bool) {
	c, err := r.scopeCredential("desktop", time.Now())
	if err != nil {
		return 0, false
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	q := queryAccountQuota(ctx, client, c)
	return handoffWindow(&q, 18000)
}
