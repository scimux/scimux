package identity

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestAddressRoundTrip(t *testing.T) {
	inst := InstallationID(validInstallationID)
	cases := []struct {
		name  string
		build func() (Address, error)
		text  string
		kind  Kind
	}{
		{
			name:  "lane",
			build: func() (Address, error) { return LaneAddress(inst, "lane-1") },
			text:  "scimux1/0123456789abcdef0123456789abcdef/lane/lane-1",
			kind:  KindLane,
		},
		{
			name:  "chat",
			build: func() (Address, error) { return ChatAddress(inst, "abcdef0123456789") },
			text:  "scimux1/0123456789abcdef0123456789abcdef/chat/abcdef0123456789",
			kind:  KindChat,
		},
		{
			name:  "bubble",
			build: func() (Address, error) { return BubbleAddress(inst, "abcdef0123456789", 1, 2) },
			text:  "scimux1/0123456789abcdef0123456789abcdef/bubble/abcdef0123456789/1/2",
			kind:  KindBubble,
		},
		{
			name:  "bubble zero",
			build: func() (Address, error) { return BubbleAddress(inst, "abcdef0123456789", 0, 0) },
			text:  "scimux1/0123456789abcdef0123456789abcdef/bubble/abcdef0123456789/0/0",
			kind:  KindBubble,
		},
		{
			name:  "bubble two digits",
			build: func() (Address, error) { return BubbleAddress(inst, "abcdef0123456789", 10, 12) },
			text:  "scimux1/0123456789abcdef0123456789abcdef/bubble/abcdef0123456789/10/12",
			kind:  KindBubble,
		},
		{
			name:  "note",
			build: func() (Address, error) { return NoteAddress(inst, "1700000000aabbccdd") },
			text:  "scimux1/0123456789abcdef0123456789abcdef/note/1700000000aabbccdd",
			kind:  KindNote,
		},
		{
			name:  "section",
			build: func() (Address, error) { return SectionAddress(inst, "1700000000aabbccdd", "1700000001aabbccde") },
			text:  "scimux1/0123456789abcdef0123456789abcdef/section/1700000000aabbccdd/1700000001aabbccde",
			kind:  KindSection,
		},
		{
			name: "reference",
			build: func() (Address, error) {
				return ReferenceAddress(inst, "1700000000aabbccdd", "1700000001aabbccde", "1700000002aabbccdf")
			},
			text: "scimux1/0123456789abcdef0123456789abcdef/reference/1700000000aabbccdd/1700000001aabbccde/1700000002aabbccdf",
			kind: KindReference,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != tc.text {
				t.Fatalf("String() = %q, want %q", got.String(), tc.text)
			}
			if got.Kind() != tc.kind {
				t.Fatalf("Kind() = %q, want %q", got.Kind(), tc.kind)
			}
			if got.Installation() != inst {
				t.Fatalf("Installation() = %q", got.Installation())
			}
			parsed, err := ParseAddress(got.String())
			if err != nil {
				t.Fatal(err)
			}
			if parsed != got {
				t.Fatalf("parsed %+v, constructed %+v", parsed, got)
			}
			if parsed.String() != tc.text {
				t.Fatalf("parsed String() = %q, want %q", parsed.String(), tc.text)
			}
			again, err := ParseAddress(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if again != got || again.String() != tc.text {
				t.Fatalf("ParseAddress(%q) = %+v %q", tc.text, again, again.String())
			}
		})
	}
}

func TestAddressLaneTextRoundTrip(t *testing.T) {
	inst := InstallationID(validInstallationID)
	const prefix = "scimux1/0123456789abcdef0123456789abcdef/lane/"
	cases := []struct {
		raw, escaped string
	}{
		{raw: "a/b", escaped: "a%2Fb"},
		{raw: "a:b", escaped: "a:b"},
		{raw: "100%", escaped: "100%25"},
		{raw: "a b", escaped: "a%20b"},
		{raw: "车道", escaped: "%E8%BD%A6%E9%81%93"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := LaneAddress(inst, tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != prefix+tc.escaped {
				t.Fatalf("String() = %q, want %q", got.String(), prefix+tc.escaped)
			}
			parsed, err := ParseAddress(prefix + tc.escaped)
			if err != nil {
				t.Fatal(err)
			}
			if parsed != got || parsed.String() != prefix+tc.escaped {
				t.Fatalf("parsed %+v %q", parsed, parsed.String())
			}
		})
	}
}

func TestLaneAddressDotDotRoundTrip(t *testing.T) {
	inst := InstallationID(validInstallationID)
	const want = "scimux1/0123456789abcdef0123456789abcdef/lane/.."
	got, err := LaneAddress(inst, "..")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Fatalf("String() = %q, want %q", got.String(), want)
	}
	parsed, err := ParseAddress(want)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != got || parsed.String() != want {
		t.Fatalf("parsed %+v %q", parsed, parsed.String())
	}
}

func TestAddressChatIDShapes(t *testing.T) {
	inst := InstallationID(validInstallationID)
	for _, uid := range []string{"abcdef0123456789", "t18f3a2b1c0"} {
		t.Run(uid, func(t *testing.T) {
			chat, err := ChatAddress(inst, uid)
			if err != nil {
				t.Fatal(err)
			}
			bubble, err := BubbleAddress(inst, uid, 0, 1)
			if err != nil {
				t.Fatal(err)
			}
			for _, addr := range []Address{chat, bubble} {
				parsed, err := ParseAddress(addr.String())
				if err != nil {
					t.Fatal(err)
				}
				if parsed != addr || parsed.String() != addr.String() || addr.String() == "" {
					t.Fatalf("round trip %+v %q", parsed, addr.String())
				}
			}
			if !strings.HasSuffix(chat.String(), "/chat/"+uid) {
				t.Fatalf("chat text %q", chat.String())
			}
		})
	}
}

func TestAddressRejectsBadParts(t *testing.T) {
	inst := InstallationID(validInstallationID)
	badInst := InstallationID("not-an-id")
	nul := "a\x00b"
	utf8 := string([]byte{0xff})
	tooLong := strings.Repeat("a", 513)
	okLong := strings.Repeat("b", 512)

	rejects := []struct {
		name  string
		build func() (Address, error)
	}{
		{"empty lane", func() (Address, error) { return LaneAddress(inst, "") }},
		{"empty chat", func() (Address, error) { return ChatAddress(inst, "") }},
		{"empty bubble chat", func() (Address, error) { return BubbleAddress(inst, "", 0, 0) }},
		{"empty note", func() (Address, error) { return NoteAddress(inst, "") }},
		{"empty section note", func() (Address, error) { return SectionAddress(inst, "", "section") }},
		{"empty section", func() (Address, error) { return SectionAddress(inst, "note", "") }},
		{"empty reference note", func() (Address, error) { return ReferenceAddress(inst, "", "section", "ref") }},
		{"empty reference section", func() (Address, error) { return ReferenceAddress(inst, "note", "", "ref") }},
		{"empty reference", func() (Address, error) { return ReferenceAddress(inst, "note", "section", "") }},
		{"nul lane", func() (Address, error) { return LaneAddress(inst, nul) }},
		{"invalid utf-8", func() (Address, error) { return LaneAddress(inst, utf8) }},
		{"513 byte lane", func() (Address, error) { return LaneAddress(inst, tooLong) }},
		{"513 byte section", func() (Address, error) { return SectionAddress(inst, "note", tooLong) }},
		{"513 byte reference", func() (Address, error) { return ReferenceAddress(inst, "note", "section", tooLong) }},
		{"bad installation", func() (Address, error) { return LaneAddress(badInst, "lane") }},
		{"negative segment", func() (Address, error) { return BubbleAddress(inst, "chat", -1, 0) }},
		{"negative record", func() (Address, error) { return BubbleAddress(inst, "chat", 0, -1) }},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.build(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	got, err := LaneAddress(inst, okLong)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.String(), "/lane/"+okLong) {
		t.Fatalf("512 byte lane = %q", got.String())
	}
	parsed, err := ParseAddress(got.String())
	if err != nil || parsed != got {
		t.Fatalf("512 byte round trip: %v %+v", err, parsed)
	}
}

func TestAddressZero(t *testing.T) {
	var zero Address
	if zero.String() != "" {
		t.Fatalf("String() = %q", zero.String())
	}
	if zero.Kind() != "" || zero.Installation() != "" {
		t.Fatalf("zero kind %q installation %q", zero.Kind(), zero.Installation())
	}
	if _, err := zero.MarshalText(); err == nil {
		t.Fatal("MarshalText succeeded for the zero address")
	}
	if _, err := ParseAddress(""); err == nil {
		t.Fatal("ParseAddress accepted the empty string")
	}
}

func TestParseAddressRejects(t *testing.T) {
	id := validInstallationID
	maxIndex := strconv.Itoa(math.MaxInt)
	overflowIndex := strconv.FormatUint(uint64(math.MaxInt)+1, 10)
	rejects := []string{
		"",
		"scimux2/" + id + "/lane/main",
		"SCIMUX1/" + id + "/lane/main",
		"scimux1/" + id + "/nope/main",
		"scimux1/" + id + "/lane",
		"scimux1/" + id + "/lane/main/extra",
		"scimux1/" + id + "/chat/main/extra",
		"scimux1/" + id + "/note/main/extra",
		"scimux1/" + id + "/section/only",
		"scimux1/" + id + "/section/note/section/extra",
		"scimux1/" + id + "/bubble/chat/1",
		"scimux1/" + id + "/bubble/chat/1/2/extra",
		"scimux1/" + id + "/reference/note/section",
		"scimux1/" + id + "/reference/note/section/ref/extra",
		"scimux1/" + id + "/bubble/chat/+5/1",
		"scimux1/" + id + "/bubble/chat/05/1",
		"scimux1/" + id + "/bubble/chat/-1/1",
		"scimux1/" + id + "/bubble/chat/1/+5",
		"scimux1/" + id + "/bubble/chat/1/05",
		"scimux1/" + id + "/bubble/chat/1/-1",
		"scimux1/" + id + "/bubble/chat/" + strings.Repeat("9", 40) + "/1",
		"scimux1/" + id + "/bubble/chat/1a/1",
		"scimux1/" + id + "/bubble/chat/" + overflowIndex + "/0",
		"scimux1/" + id + "/bubble/chat/0/" + overflowIndex,
		"scimux1/" + id + "/lane/a%2fb",
		"scimux1/" + id + "/lane/a b",
		"scimux1/" + id + "/lane/a?b",
		"scimux1/" + id + "/lane/main/",
		"scimux1/" + id + "/lane/%",
		"scimux1/" + id + "/lane/%2",
		"scimux1/" + id + "/lane/%GG",
		"scimux1/" + id + "/lane/%00",
		"scimux1/" + id + "/lane/%FF",
		"scimux1/" + id + "/lane/" + strings.Repeat("a", 513),
		"scimux1/" + id + "/lane/",
		"scimux1/" + id + "/section//section",
		"scimux1/" + id + "/section/note/",
		"scimux1/" + id + "/bubble//0/0",
		"scimux1/" + id + "/bubble/chat//0",
		"scimux1/" + id + "/reference/note//ref",
		"scimux1/" + id + "/reference/note/section/",
		"scimux1/zz/lane/main",
		"scimux1/" + strings.ToUpper(id) + "/lane/main",
	}
	for _, s := range rejects {
		if _, err := ParseAddress(s); err == nil {
			t.Errorf("ParseAddress(%q) succeeded", s)
		}
	}

	accepts := []string{
		"scimux1/" + id + "/lane/a%2Fb",
		"scimux1/" + id + "/lane/%E8%BD%A6%E9%81%93",
		"scimux1/" + id + "/bubble/abcdef0123456789/0/0",
		"scimux1/" + id + "/bubble/chat/" + maxIndex + "/0",
		"scimux1/" + id + "/bubble/chat/0/" + maxIndex,
	}
	for _, s := range accepts {
		got, err := ParseAddress(s)
		if err != nil {
			t.Fatalf("ParseAddress(%q): %v", s, err)
		}
		if got.String() != s {
			t.Fatalf("String() = %q, input %q", got.String(), s)
		}
	}
}

func TestAddressDistinctness(t *testing.T) {
	first := InstallationID(validInstallationID)
	second := InstallationID(otherInstallationID)
	local := "abcdef0123456789"
	chat, err := ChatAddress(first, local)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ChatAddress(second, local)
	if err != nil {
		t.Fatal(err)
	}
	if chat == other {
		t.Fatal("the same local id under two installations compared equal")
	}
	lane, err := LaneAddress(first, local)
	if err != nil {
		t.Fatal(err)
	}
	if lane == chat {
		t.Fatal("the same local id under two kinds compared equal")
	}
	if chat.String() == other.String() || chat.String() == lane.String() {
		t.Fatalf("texts %q %q %q", chat.String(), other.String(), lane.String())
	}
}

func TestAddressTextAndJSON(t *testing.T) {
	inst := InstallationID(validInstallationID)
	addr, err := ChatAddress(inst, "abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if addr.String() == "" {
		t.Fatal("address text is empty")
	}
	text, err := addr.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != addr.String() {
		t.Fatalf("MarshalText = %q, String = %q", text, addr.String())
	}
	var back Address
	if err := back.UnmarshalText(text); err != nil {
		t.Fatal(err)
	}
	if back != addr {
		t.Fatalf("unmarshaled %+v", back)
	}
	if err := back.UnmarshalText([]byte("not an address")); err == nil {
		t.Fatal("UnmarshalText accepted invalid text")
	}
	if back != addr {
		t.Fatal("failed UnmarshalText changed the address")
	}

	wrapped := struct {
		A Address `json:"a"`
	}{A: addr}
	raw, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		A Address `json:"a"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.A != addr {
		t.Fatalf("json field = %+v", got.A)
	}

	lane, err := LaneAddress(inst, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	values := map[Address]string{addr: "chat", lane: "lane"}
	raw, err = json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var out map[Address]string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out[addr] != "chat" || out[lane] != "lane" {
		t.Fatalf("json map = %#v", out)
	}
}

func FuzzParseAddress(f *testing.F) {
	seeds := []string{
		"scimux1/0123456789abcdef0123456789abcdef/lane/main",
		"scimux1/0123456789abcdef0123456789abcdef/lane/a%2Fb",
		"scimux1/0123456789abcdef0123456789abcdef/lane/a:b",
		"scimux1/0123456789abcdef0123456789abcdef/lane/a%20b",
		"scimux1/0123456789abcdef0123456789abcdef/lane/%E8%BD%A6%E9%81%93",
		"scimux1/0123456789abcdef0123456789abcdef/chat/abcdef0123456789",
		"scimux1/0123456789abcdef0123456789abcdef/chat/t18f3a2b1c0",
		"scimux1/0123456789abcdef0123456789abcdef/bubble/abcdef0123456789/0/0",
		"scimux1/0123456789abcdef0123456789abcdef/bubble/abcdef0123456789/10/12",
		"scimux1/0123456789abcdef0123456789abcdef/note/1700000000aabbccdd",
		"scimux1/0123456789abcdef0123456789abcdef/section/1700000000aabbccdd/1700000001aabbccde",
		"scimux1/0123456789abcdef0123456789abcdef/reference/1700000000aabbccdd/1700000001aabbccde/1700000002aabbccdf",
		"",
		"nope",
		"scimux1/0123456789abcdef0123456789abcdef/lane/a%2fb",
		"scimux1/0123456789abcdef0123456789abcdef/bubble/chat/+5/1",
		"scimux1/0123456789abcdef0123456789abcdef/bubble/chat/05/0",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := ParseAddress(s)
		if err != nil {
			return
		}
		if got.String() != s {
			t.Fatalf("String() = %q, input %q", got.String(), s)
		}
	})
}
