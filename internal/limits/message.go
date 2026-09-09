// Package limits shares form limits between the browser and message handling.
package limits

const MaxMessageUnits = 100000

// A BMP character can occupy three UTF-8 bytes, each percent encoded in a form.
// Allow additional space for the other form fields and their names.
const MaxFormBytes = 9*MaxMessageUnits + 16*1024

// MessageFits counts UTF-16 code units, as HTML textarea maxlength does.
func MessageFits(text string) bool {
	units := 0
	for _, r := range text {
		units++
		if r > 0xffff {
			units++
		}
		if units > MaxMessageUnits {
			return false
		}
	}
	return true
}

// Attachment limits are also exposed to the composer. Image size uses decimal
// bytes to stay below providers' 5 MB limit, before base64 encoding.
const (
	MaxAttachments           = 5
	MaxImageBytes      int64 = 5_000_000
	MaxDocumentBytes   int64 = 20 * 1024 * 1024
	MaxAttachmentBytes int64 = 25 * 1024 * 1024
	MaxUploadBytes     int64 = MaxAttachmentBytes + MaxFormBytes + 64*1024
	MaxImageDimension        = 8000
	MaxImagePixels           = 16_000_000
)
