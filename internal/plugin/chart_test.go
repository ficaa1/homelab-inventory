package plugin

import (
	"math"
	"testing"
)

func TestChartValidation(t *testing.T) {
	one, two, value := 1.0, 2.0, 3.0
	good := Block{Type: "chart", Unit: "bytes_s", Series: []Series{{Name: "Receive", Points: []Point{{&one, &value}, {&two, nil}}}}}
	for _, mutate := range []func(*Block){
		func(b *Block) { n := math.NaN(); b.Series[0].Points[0][1] = &n },
		func(b *Block) { n := math.Inf(1); b.Series[0].Points[0][0] = &n },
		func(b *Block) { b.Series[0].Points[1][0] = &one },
		func(b *Block) { b.Series[0].Points[0][0] = &two; b.Series[0].Points[1][0] = &one },
		func(b *Block) { b.Series[0].Points = make([]Point, 501) },
		func(b *Block) { b.Series = make([]Series, 9) },
	} {
		b := good
		b.Series = append([]Series{}, good.Series...)
		b.Series[0].Points = append([]Point{}, good.Series[0].Points...)
		mutate(&b)
		if _, err := ValidateView(View{Blocks: []Block{b}}, Manifest{}); err == nil {
			t.Fatal("invalid chart accepted")
		}
	}
	if _, err := ValidateView(View{Blocks: []Block{{Type: "section", Blocks: []Block{good}}, {Type: "table", Columns: []Column{{Key: "rate", Format: "bytes_s"}}}}}, Manifest{}); err != nil {
		t.Fatal(err)
	}
}
