package wamd

import "testing"

func TestHTML(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold**, *it*, _it_, ~~gone~~ and `<b>x</b>`",
			"<p><strong>bold</strong>, <em>it</em>, <em>it</em>, <del>gone</del> and <code>&lt;b&gt;x&lt;/b&gt;</code></p>\n"},
		{"snake_case_name and 2 * 3 * 4", "<p>snake_case_name and 2 * 3 * 4</p>\n"},
		{"## Summary\nline one\nline two\n\nnext", "<h2>Summary</h2>\n<p>line one<br>line two</p>\n<p>next</p>\n"},
		{"see [the docs](https://x.dev/?a=1&b=2) ![chart](c.png)", "<p>see <u>the docs</u> [chart]</p>\n"},
		{"<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>\n"},
		{"```go\nif a < b {\n```", "<pre><code>if a &lt; b {</code></pre>\n"},
		{"| Name | Time |\n|---|--:|\n| **build** | 3s |\n| tests |",
			"<table>\n<thead><tr><th>Name</th><th style=\"text-align:right\">Time</th></tr></thead>\n<tbody>\n" +
				"<tr><td><strong>build</strong></td><td style=\"text-align:right\">3s</td></tr>\n<tr><td>tests</td><td style=\"text-align:right\"></td></tr>\n</tbody>\n</table>\n"},
		{"- one\n- two\n  - nested\n  more\n- [x] done\n\nafter",
			"<ul>\n<li>one</li>\n<li>two<ul>\n<li>nested<br>more</li>\n</ul>\n</li>\n<li>☑ done</li>\n</ul>\n<p>after</p>\n"},
		{"3. three\n4. four", "<ol start=\"3\">\n<li>three</li>\n<li>four</li>\n</ol>\n"},
		{"> quoted **bit**\n> more\n\n---", "<blockquote>\n<p>quoted <strong>bit</strong><br>more</p>\n</blockquote>\n<hr>\n"},
		{"a `b", "<p>a `b</p>\n"},
	}
	for _, c := range cases {
		if got := HTML(c.in); got != c.want {
			t.Errorf("HTML(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestWidest(t *testing.T) {
	if got := Widest("short\n\n| a | bb |\n|---|---|\n| cccc | d |\n"); got != 4+3+2+3 {
		t.Errorf("table = %d", got)
	}
	if got := Widest("```\n" + "0123456789\n```\n| x |"); got != 10 {
		t.Errorf("code = %d", got)
	}
}
