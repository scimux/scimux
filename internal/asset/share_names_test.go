package asset

import "testing"

func TestShareNamesDropsLocalPaths(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"no marker", "plain text", "plain text"},
		{"stored file name", "read this\n\n[attached file: /srv/owner-data/attachments/n1/0a1b2c3d-notes.txt]", "read this\n\n[attached file: notes.txt]"},
		{"image and file", "[attached image: /srv/x/deadbeef-plot.png]\n[attached file: /srv/x/report.pdf]", "[attached image: plot.png]\n[attached file: report.pdf]"},
		{"prefix only", "[attached file: /srv/x/0a1b2c3d-]", "[attached file: 0a1b2c3d-]"},
		{"not a stored prefix", "[attached file: /srv/x/0A1B2C3D-notes.txt]", "[attached file: 0A1B2C3D-notes.txt]"},
		{"relative path", "[attached file: notes/a-b.txt]", "[attached file: a-b.txt]"},
		{"other bracket text", "[attached link: /srv/x/a]", "[attached link: /srv/x/a]"},
		{"space in name", "[attached file: /srv/x/0a1b2c3d-lab notes.txt]", "[attached file: lab notes.txt]"},
		{"bracket in directory", "[attached file: /srv/owner]data/attachments/n1/0a1b2c3d-a.txt]", "[attached file: a.txt]"},
		{"bracket in name", "[attached file: /srv/x/0a1b2c3d-a]b.txt]\nnext", "[attached file: a]b.txt]\nnext"},
		{"newline in name", "[attached file: /srv/x/0a1b2c3d-a\nb.txt]", "[attached file: a\nb.txt]"},
		{"trailing slash", "[attached file: /srv/x/]", "[attached file: x]"},
		{"two markers on one line", "[attached file: /srv/a/x.txt] [attached file: /srv/b/y.txt]", "[attached file: y.txt]"},
		{"typed inline text stays as typed", "see [attached file: notes.txt] above", "see [attached file: notes.txt] above"},
	} {
		if got := ShareNames(tc.in); got != tc.want {
			t.Errorf("%s: ShareNames(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
