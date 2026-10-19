package api

import (
	"net/url"
	"strconv"

	"github.com/lejianwen/rustdesk-api/v2/internal/clientcompat"
)

// ClientPage accepts native current and the older page alias. A disagreement
// is rejected rather than silently returning a repeated page.
func ClientPage(query url.Values) (clientcompat.Page, error) {
	page := clientcompat.Page{Current: 1, Size: 100}
	for _, key := range []string{"current", "page", "pageSize"} {
		if len(query[key]) > 1 {
			return page, clientcompat.ErrInput
		}
	}
	parse := func(key string, defaultValue int) (int, error) {
		values, ok := query[key]
		if !ok {
			return defaultValue, nil
		}
		if len(values) != 1 {
			return 0, clientcompat.ErrInput
		}
		v, err := strconv.Atoi(values[0])
		if err != nil || v <= 0 {
			return 0, clientcompat.ErrInput
		}
		return v, nil
	}
	current, err := parse("current", 1)
	if err != nil {
		return page, err
	}
	legacy, err := parse("page", current)
	if err != nil {
		return page, err
	}
	if query.Has("current") && query.Has("page") && current != legacy {
		return page, clientcompat.ErrInput
	}
	if !query.Has("current") {
		current = legacy
	}
	size, err := parse("pageSize", 100)
	if err != nil || size > 1000 || (current-1) > int(^uint(0)>>1)/size {
		return page, clientcompat.ErrInput
	}
	return clientcompat.Page{Current: current, Size: size}, nil
}
