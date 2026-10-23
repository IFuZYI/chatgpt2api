package protocol

import (
	"encoding/json"
	"os"
	"testing"
)

type maskGoldenCase struct {
	Name             string  `json:"name"`
	MaskBase64       string  `json:"mask_b64"`
	MaskFilename     string  `json:"mask_filename"`
	ExpectedSize     []int   `json:"expected_size"`
	ExpectedAlpha    []uint8 `json:"expected_alpha"`
	ExpectedMime     string  `json:"expected_mime"`
	ExpectedFilename string  `json:"expected_filename"`
	Exact            bool    `json:"exact"`
}

type maskGoldenFile struct {
	Note           string           `json:"note"`
	SourceBase64   string           `json:"source_b64"`
	SourceFilename string           `json:"source_filename"`
	SourceMime     string           `json:"source_mime"`
	Cases          []maskGoldenCase `json:"cases"`
}

func loadMaskGolden(t *testing.T) maskGoldenFile {
	t.Helper()
	payload, err := os.ReadFile("testdata/mask_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var golden maskGoldenFile
	if err := json.Unmarshal(payload, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(golden.Cases) == 0 {
		t.Fatal("golden has no cases")
	}
	return golden
}

func TestMaskMatchesPythonGolden(t *testing.T) {
	golden := loadMaskGolden(t)
	for _, tc := range golden.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			images := [][3]string{{golden.SourceBase64, golden.SourceFilename, golden.SourceMime}}
			masks := [][3]string{{tc.MaskBase64, tc.MaskFilename, "image/png"}}

			got, err := CompositeMask(images, masks)
			if err != nil {
				t.Fatalf("CompositeMask: %v", err)
			}
			if got[0][1] != tc.ExpectedFilename {
				t.Errorf("filename = %q, want %q", got[0][1], tc.ExpectedFilename)
			}
			if got[0][2] != tc.ExpectedMime {
				t.Errorf("mime = %q, want %q", got[0][2], tc.ExpectedMime)
			}

			decoded := decodePNG(t, got[0])
			bounds := decoded.Bounds()
			if bounds.Dx() != tc.ExpectedSize[0] || bounds.Dy() != tc.ExpectedSize[1] {
				t.Fatalf("size = %dx%d, want %dx%d",
					bounds.Dx(), bounds.Dy(), tc.ExpectedSize[0], tc.ExpectedSize[1])
			}

			index := 0
			mismatches := 0
			for y := 0; y < bounds.Dy(); y++ {
				for x := 0; x < bounds.Dx(); x++ {
					want := tc.ExpectedAlpha[index]
					index++
					gotAlpha := alphaAt(decoded, x, y)
					if tc.Exact {
						if gotAlpha != want {
							mismatches++
							if mismatches <= 8 {
								t.Errorf("alpha at (%d,%d) = %d, want %d", x, y, gotAlpha, want)
							}
						}
						continue
					}
					diff := int(gotAlpha) - int(want)
					if diff < 0 {
						diff = -diff
					}
					if diff > 16 {
						mismatches++
						if mismatches <= 8 {
							t.Errorf("alpha at (%d,%d) = %d, want ~%d (diff %d)", x, y, gotAlpha, want, diff)
						}
					}
				}
			}
			if mismatches > 0 {
				t.Errorf("%d/%d pixels mismatch", mismatches, len(tc.ExpectedAlpha))
			}
		})
	}
}
