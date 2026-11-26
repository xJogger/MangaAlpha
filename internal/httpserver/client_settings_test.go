package httpserver

import "testing"

func TestValidateClientSettingWebUIDisplayRanges(t *testing.T) {
	value := func(s string) *string { return &s }
	cases := []struct {
		name    string
		key     string
		value   *string
		wantErr bool
	}{
		{"page size min", "webui.shelf.pageSize", value("12"), false},
		{"page size max", "webui.shelf.pageSize", value("120"), false},
		{"page size default", "webui.shelf.pageSize", value("36"), false},
		{"page size below range", "webui.shelf.pageSize", value("11"), true},
		{"page size above range", "webui.shelf.pageSize", value("121"), true},
		{"page size not a number", "webui.shelf.pageSize", value("abc"), true},
		{"page size deletion", "webui.shelf.pageSize", nil, false},
		{"home updated in range", "webui.home.updatedSize", value("18"), false},
		{"home added above range", "webui.home.addedSize", value("37"), true},
		{"home continue below range", "webui.home.continueSize", value("3"), true},
		{"infinite scroll enabled", "webui.shelf.infiniteScroll", value("1"), false},
		{"infinite scroll disabled", "webui.shelf.infiniteScroll", value("0"), false},
		{"infinite scroll boolean text", "webui.shelf.infiniteScroll", value("true"), true},
		{"infinite scroll out of range", "webui.shelf.infiniteScroll", value("2"), true},
		{"infinite scroll empty value", "webui.shelf.infiniteScroll", value(""), true},
		{"infinite scroll deletion", "webui.shelf.infiniteScroll", nil, false},
		{"unrelated webui key untouched", "webui.reader.mode", value("rtl"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClientSetting(tc.key, tc.value)
			display := "<nil>"
			if tc.value != nil {
				display = *tc.value
			}
			if tc.wantErr && err == nil {
				t.Fatalf("validateClientSetting(%q) = nil, want error", display)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateClientSetting(%q) = %v, want nil", display, err)
			}
		})
	}
}
