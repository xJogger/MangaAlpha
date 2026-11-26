package bangumi

import (
	"testing"
)

func TestMatchBooksToTankobonsVolumePatternsAndMissingVolumes(t *testing.T) {
	tankobons := []TankobonSubject{
		{SubjectID: 1001, Name: "葬送のフリーレン (1)", NameCN: "葬送的芙莉莲 (1)", Date: "2020-08-18", ISBN: "978-4098501809", Order: 1},
		{SubjectID: 1002, Name: "葬送のフリーレン (2)", NameCN: "葬送的芙莉莲 (2)", Date: "2020-10-16", ISBN: "978-4098501816", Order: 2},
		{SubjectID: 1003, Name: "葬送のフリーレン 03", NameCN: "葬送的芙莉莲 第3卷", Date: "2020-12-18", ISBN: "978-4098502851", Order: 3},
		{SubjectID: 1004, Name: "葬送のフリーレン Vol.4", NameCN: "", Date: "2021-03-17", ISBN: "978-4098504909", Order: 4},
	}

	// Missing volumes (local only has Vol.03 and Vol.04): MUST bind to 1003 and 1004, NEVER shift to 1001/1002!
	missingBooks := []VolumeMatchBook{
		{BookID: "b3", Name: "[山田钟人] 葬送的芙莉莲 Vol.03 (2023) [1080p].cbz"},
		{BookID: "b4", Name: "葬送的芙莉莲 第4卷 [东立][200P].zip"},
	}
	got := MatchBooksToTankobons(500, false, missingBooks, tankobons)
	if len(got) != 2 {
		t.Fatalf("expected 2 matches for missing volumes test, got %+v", got)
	}
	if got[0].BookID != "b3" || got[0].SubjectID != 1003 || got[0].MatchedBy != "volume_number" || got[0].ISBN != "978-4098502851" {
		t.Fatalf("unexpected match for b3: %+v", got[0])
	}
	if got[1].BookID != "b4" || got[1].SubjectID != 1004 || got[1].MatchedBy != "volume_number" || got[1].Title != "葬送のフリーレン Vol.4" {
		t.Fatalf("unexpected match for b4: %+v", got[1])
	}

	// Omnibus range (Vol.01-02) extracts starting volume 1, bracketed/trailing numbers, v-prefix, and fallback book.Number.
	mixedBooks := []VolumeMatchBook{
		{BookID: "omni", Name: "葬送的芙莉莲 Vol.01-02.cbz"},
		{BookID: "paren", Name: "葬送的芙莉莲 (02) [1600px].cbz"},
		{BookID: "vpref", Name: "葬送的芙莉莲 v3.rar"},
		{BookID: "numfield", Name: "葬送的芙莉莲.cbz", Number: "4"},
	}
	mixedGot := MatchBooksToTankobons(500, false, mixedBooks, tankobons)
	wantIDs := map[string]int64{"omni": 1001, "paren": 1002, "vpref": 1003, "numfield": 1004}
	if len(mixedGot) != len(wantIDs) {
		t.Fatalf("expected %d mixed matches, got %+v", len(wantIDs), mixedGot)
	}
	for _, r := range mixedGot {
		if r.SubjectID != wantIDs[r.BookID] {
			t.Fatalf("book %s matched subject %d, want %d", r.BookID, r.SubjectID, wantIDs[r.BookID])
		}
	}

	// Noise-only filenames with year/resolution (no volume number) must NOT falsely match Vol.2023 or Vol.1080!
	noiseBooks := []VolumeMatchBook{
		{BookID: "noise1", Name: "葬送的芙莉莲 (2023) [1080p][180P].cbz"},
	}
	if res := MatchBooksToTankobons(500, false, noiseBooks, tankobons); len(res) != 0 {
		t.Fatalf("expected year/resolution noise not to match, got %+v", res)
	}
}

func TestMatchBooksToTankobonsBlocksSingleChapters(t *testing.T) {
	tankobons := []TankobonSubject{
		{SubjectID: 2001, NameCN: "海贼王 (1)", Order: 1},
		{SubjectID: 2002, NameCN: "海贼王 (2)", Order: 2},
		{SubjectID: 2003, NameCN: "海贼王 (3)", Order: 3},
	}

	books := []VolumeMatchBook{
		{BookID: "ch1", Name: "第01话.cbz", Number: "1"},
		{BookID: "ch2", Name: "海贼王 02話.zip"},
		{BookID: "ch3", Name: "海贼王 第3回.cbz"},
		{BookID: "ch4", Name: "One Piece Ch.01.cbz"},
		{BookID: "ch5", Name: "One_Piece_Chapter_02.cbz"},
		{BookID: "ch6", Name: "One Piece Ep.03.cbz"},
		{BookID: "ch7", Name: "One Piece Episode 1.cbz"},
		// Has explicit volume marker alongside chapter marker -> allowed and matches Vol 2!
		{BookID: "vol_ch", Name: "海贼王 Vol.02 Ch.12.cbz"},
	}

	got := MatchBooksToTankobons(600, false, books, tankobons)
	if len(got) != 1 || got[0].BookID != "vol_ch" || got[0].SubjectID != 2002 {
		t.Fatalf("expected only vol_ch to match tankobon 2002, got %+v", got)
	}
}

func TestMatchBooksToTankobonsExtrasAndPartsAndOrderFallback(t *testing.T) {
	// 1. Two-part 上/下 & 前篇/后篇 + Extra matching by subtitle (never mis-binding regular volumes).
	tankobons := []TankobonSubject{
		{SubjectID: 3001, Name: "作品 (上)", NameCN: "测试作品 (上)", Order: 1},
		{SubjectID: 3002, Name: "作品 (下)", NameCN: "测试作品 (下)", Order: 2},
		{SubjectID: 3003, Name: "作品 公式ガイド", NameCN: "测试作品 公式书", Order: 3},
		{SubjectID: 3004, Name: "作品 外伝", NameCN: "测试作品 番外篇", Order: 4},
	}
	books := []VolumeMatchBook{
		{BookID: "b_up", Name: "测试作品 上.cbz"},
		{BookID: "b_down", Name: "测试作品 下.cbz"},
		{BookID: "b_guide", Name: "测试作品 公式书.cbz"},
		{BookID: "b_extra", Name: "番外篇.cbz"},
		// Unmatched extra (even with Vol.01 in name) MUST NOT hijack volume 1!
		{BookID: "b_unmatched_sp", Name: "测试作品 Vol.01 特典小册子.cbz"},
	}
	got := MatchBooksToTankobons(700, false, books, tankobons)
	if len(got) != 4 {
		t.Fatalf("expected 4 matches, got %+v", got)
	}
	want := map[string]struct {
		id int64
		by string
	}{
		"b_up":    {3001, "volume_number"},
		"b_down":  {3002, "volume_number"},
		"b_guide": {3003, "title"},
		"b_extra": {3004, "title"},
	}
	for _, r := range got {
		w, ok := want[r.BookID]
		if !ok || r.SubjectID != w.id || r.MatchedBy != w.by {
			t.Fatalf("unexpected match result %+v", r)
		}
	}

	// 2. Three-part 上/中/下 mapped against numbered tankobons 1/2/3.
	triTankobons := []TankobonSubject{
		{SubjectID: 3101, NameCN: "三部曲 (1)", Order: 1},
		{SubjectID: 3102, NameCN: "三部曲 (2)", Order: 2},
		{SubjectID: 3103, NameCN: "三部曲 (3)", Order: 3},
	}
	triBooks := []VolumeMatchBook{
		{BookID: "t1", Name: "三部曲 上卷.cbz"},
		{BookID: "t2", Name: "三部曲 中卷.cbz"},
		{BookID: "t3", Name: "三部曲 下卷.cbz"},
	}
	triGot := MatchBooksToTankobons(710, false, triBooks, triTankobons)
	if len(triGot) != 3 || triGot[0].SubjectID != 3101 || triGot[1].SubjectID != 3102 || triGot[2].SubjectID != 3103 {
		t.Fatalf("unexpected 上/中/下 match: %+v", triGot)
	}

	// 3. 前篇/后篇 mapped to 1/2.
	frontBackGot := MatchBooksToTankobons(710, false, []VolumeMatchBook{
		{BookID: "f", Name: "三部曲 前篇.cbz"},
		{BookID: "b", Name: "三部曲 后篇.cbz"},
	}, triTankobons[:2])
	if len(frontBackGot) != 2 || frontBackGot[0].SubjectID != 3101 || frontBackGot[1].SubjectID != 3102 {
		t.Fatalf("unexpected 前篇/后篇 match: %+v", frontBackGot)
	}

	// 4. Order fallback when tankobon titles lack explicit volume numbers and Order is consistent/monotonic.
	untitledTankobons := []TankobonSubject{
		{SubjectID: 4001, NameCN: "序幕之章", Order: 1},
		{SubjectID: 4002, NameCN: "觉醒之章", Order: 2},
		{SubjectID: 4003, NameCN: "终焉之章", Order: 3},
	}
	ordGot := MatchBooksToTankobons(800, false, []VolumeMatchBook{
		{BookID: "o1", Name: "Vol.01.cbz"},
		{BookID: "o3", Name: "Vol.03.cbz"},
	}, untitledTankobons)
	if len(ordGot) != 2 || ordGot[0].SubjectID != 4001 || ordGot[1].SubjectID != 4003 {
		t.Fatalf("unexpected monotonic Order fallback match: %+v", ordGot)
	}

	// 5. Inconsistent Order (e.g. Order 1 has Vol.2, Order 2 is untitled) MUST NOT blindly assign Vol.2 to Order 2.
	inconsistentTankobons := []TankobonSubject{
		{SubjectID: 4101, NameCN: "漫画 (2)", Order: 1},
		{SubjectID: 4102, NameCN: "未知副标题", Order: 2},
	}
	incGot := MatchBooksToTankobons(810, false, []VolumeMatchBook{
		{BookID: "i2", Name: "漫画 Vol.02.cbz"},
	}, inconsistentTankobons)
	if len(incGot) != 1 || incGot[0].SubjectID != 4101 {
		t.Fatalf("expected explicit Vol.2 to win 4101 without Order collision, got %+v", incGot)
	}
}

func TestMatchBooksToTankobonsEmptyTankobonsPolicies(t *testing.T) {
	// One-shot / single-book series with no tankobons -> fallback to parentSubjectID.
	oneBook := []VolumeMatchBook{{BookID: "single", Name: "短篇集.cbz"}}
	resSingle := MatchBooksToTankobons(9001, true, oneBook, nil)
	if len(resSingle) != 1 || resSingle[0].SubjectID != 9001 || resSingle[0].MatchedBy != "oneshot_fallback" ||
		resSingle[0].CoverURL != "https://api.bgm.tv/v0/subjects/9001/image?type=common" {
		t.Fatalf("unexpected oneshot fallback: %+v", resSingle)
	}

	// Single book in a non-oneshot series with no tankobons also falls back (unless single-chapter file).
	resOneVol := MatchBooksToTankobons(9002, false, []VolumeMatchBook{{BookID: "v1", Name: "全一卷.cbz"}}, nil)
	if len(resOneVol) != 1 || resOneVol[0].SubjectID != 9002 || resOneVol[0].MatchedBy != "oneshot_fallback" {
		t.Fatalf("unexpected single-book fallback: %+v", resOneVol)
	}

	// Multi-book series with no tankobons -> MUST return empty (never stamp series cover onto every volume).
	multiBooks := []VolumeMatchBook{
		{BookID: "m1", Name: "Vol.01.cbz"},
		{BookID: "m2", Name: "Vol.02.cbz"},
	}
	if resMulti := MatchBooksToTankobons(9003, false, multiBooks, nil); len(resMulti) != 0 {
		t.Fatalf("expected 0 matches for multi-volume series without tankobons, got %+v", resMulti)
	}
}
