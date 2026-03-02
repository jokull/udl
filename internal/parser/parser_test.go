package parser

import (
	"testing"

	"github.com/jokull/udl/internal/quality"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input string
		want  Result
	}{
		{
			input: "The.Last.of.Us.S01E03.1080p.WEB-DL.DD5.1.H.264-GROUP",
			want: Result{
				Title:   "The Last of Us",
				Season:  1,
				Episode: 3,
				Quality: quality.WEBDL1080p,
				Source:  "WEB-DL",
				Res:     "1080p",
				Group:   "GROUP",
				IsTV:    true,
			},
		},
		{
			input: "Severance.S02E01.720p.HDTV.x264-FLEET",
			want: Result{
				Title:   "Severance",
				Season:  2,
				Episode: 1,
				Quality: quality.HDTV720p,
				Source:  "HDTV",
				Res:     "720p",
				Group:   "FLEET",
				IsTV:    true,
			},
		},
		{
			input: "Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.H.265-GROUP",
			want: Result{
				Title:   "Dune Part Two",
				Year:    2024,
				Season:  -1,
				Episode: -1,
				Quality: quality.WEBDL2160p,
				Source:  "WEB-DL",
				Res:     "2160p",
				Group:   "GROUP",
				IsTV:    false,
			},
		},
		{
			input: "Movie.Name.2024.1080p.BluRay.REMUX.AVC.DTS-HD.MA.5.1-FGT",
			want: Result{
				Title:   "Movie Name",
				Year:    2024,
				Season:  -1,
				Episode: -1,
				Quality: quality.Remux1080p,
				Source:  "REMUX",
				Res:     "1080p",
				Group:   "FGT",
				IsTV:    false,
			},
		},
		{
			input: "The.Office.US.S05E14E15.1080p.WEB-DL-GROUP",
			want: Result{
				Title:   "The Office US",
				Season:  5,
				Episode: 14,
				Quality: quality.WEBDL1080p,
				Source:  "WEB-DL",
				Res:     "1080p",
				Group:   "GROUP",
				IsTV:    true,
			},
		},
		{
			input: "Some.Show.S03.1080p.WEB-DL-GROUP",
			want: Result{
				Title:   "Some Show",
				Season:  3,
				Episode: -1,
				Quality: quality.WEBDL1080p,
				Source:  "WEB-DL",
				Res:     "1080p",
				Group:   "GROUP",
				IsTV:    true,
			},
		},
		{
			input: "Movie.2023.720p.BluRay.x264-GROUP",
			want: Result{
				Title:   "Movie",
				Year:    2023,
				Season:  -1,
				Episode: -1,
				Quality: quality.Bluray720p,
				Source:  "BluRay",
				Res:     "720p",
				Group:   "GROUP",
				IsTV:    false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := Parse(tc.input)

			if got.Title != tc.want.Title {
				t.Errorf("Title: got %q, want %q", got.Title, tc.want.Title)
			}
			if got.Year != tc.want.Year {
				t.Errorf("Year: got %d, want %d", got.Year, tc.want.Year)
			}
			if got.Season != tc.want.Season {
				t.Errorf("Season: got %d, want %d", got.Season, tc.want.Season)
			}
			if got.Episode != tc.want.Episode {
				t.Errorf("Episode: got %d, want %d", got.Episode, tc.want.Episode)
			}
			if got.Quality != tc.want.Quality {
				t.Errorf("Quality: got %v, want %v", got.Quality, tc.want.Quality)
			}
			if got.Source != tc.want.Source {
				t.Errorf("Source: got %q, want %q", got.Source, tc.want.Source)
			}
			if got.Res != tc.want.Res {
				t.Errorf("Res: got %q, want %q", got.Res, tc.want.Res)
			}
			if got.Group != tc.want.Group {
				t.Errorf("Group: got %q, want %q", got.Group, tc.want.Group)
			}
			if got.IsTV != tc.want.IsTV {
				t.Errorf("IsTV: got %v, want %v", got.IsTV, tc.want.IsTV)
			}
		})
	}
}
