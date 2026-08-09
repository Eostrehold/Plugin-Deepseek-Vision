package safety

import "testing"

func TestValidateImageReferences(t *testing.T) {
	l := Limits{MaxImageReferenceBytes: 200}
	valid := []string{
		"https://example.com/a.png",
		"HTTP://LOCALHOST/image",
		"data:image/png;base64,AAAA",
		"DATA:image/svg+xml,%3Csvg%3E%3C/svg%3E",
	}
	for _, ref := range valid {
		if err := l.ValidateImageReference(ref); err != nil {
			t.Errorf("valid %q: %v", ref, err)
		}
	}
	invalid := []string{
		"",
		"file:///tmp/a",
		"javascript:alert(1)",
		"https://user:pass@example.com/a",
		"https:///missing-host.png",
		"https://example.com/%zz",
		"http://127.0.0.1/image.png",
		"http://10.0.0.8/image.png",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/image.png",
		"http://127.0.0.1./image.png",
		"http://[fe80::1%25eth0]/image.png",
		"http://2130706433/image.png",
		"http://0x7f000001/image.png",
		"http://0177.0.0.1/image.png",
		"data:text/plain,hello",
		"data:image/*,wildcard",
		"data:image/png;base64,%%%",
		"data:image/png;base64,",
		"data:image/png;BASE64;charset=utf-8,AAAA",
		"data:image/png,invalid%zz",
	}
	for _, ref := range invalid {
		if err := l.ValidateImageReference(ref); err == nil {
			t.Errorf("invalid %q accepted", ref)
		}
	}
}

func TestValidateImageReferenceConfiguredLimit(t *testing.T) {
	ref := "https://example.com/large.png"
	if err := ValidateImageReference(ref, len(ref)-1); err != ErrImageReferenceTooLarge {
		t.Fatalf("small limit error = %v", err)
	}
	if err := ValidateImageReference(ref, len(ref)); err != nil {
		t.Fatalf("exact limit rejected: %v", err)
	}
}
