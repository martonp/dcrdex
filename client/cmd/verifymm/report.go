// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"os"
	"time"

	"github.com/go-pdf/fpdf"
	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
)

type reportGraphs struct {
	InBandBuy  string
	InBandSell string
	TotalBuy   string
	TotalSell  string
	Prices     string
}

func runGenReport() error {
	var reportFile, outputFile string
	fs := flag.NewFlagSet("genreport", flag.ExitOnError)
	fs.StringVar(&reportFile, "report", "", "Path to the JSON coverage report file (required)")
	fs.StringVar(&outputFile, "out", "coverage.pdf", "Output PDF file")
	fs.Parse(os.Args[1:])

	if reportFile == "" {
		return errors.New("report file is required (-report)")
	}

	reportData, err := os.ReadFile(reportFile)
	if err != nil {
		return fmt.Errorf("failed to read report file: %w", err)
	}

	var report CoverageReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		return fmt.Errorf("failed to parse report JSON: %w", err)
	}

	if len(report.TimeSeries) < 2 {
		return errors.New("report contains insufficient time series data")
	}

	// Generate graphs to temp files.
	mkTmp := func(prefix string) (string, error) {
		f, err := os.CreateTemp("", prefix+"-*.png")
		if err != nil {
			return "", err
		}
		path := f.Name()
		f.Close()
		return path, nil
	}

	var graphs reportGraphs
	if graphs.InBandBuy, err = mkTmp("inband-buy"); err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if graphs.InBandSell, err = mkTmp("inband-sell"); err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if graphs.TotalBuy, err = mkTmp("total-buy"); err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if graphs.TotalSell, err = mkTmp("total-sell"); err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if graphs.Prices, err = mkTmp("prices"); err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	defer os.Remove(graphs.InBandBuy)
	defer os.Remove(graphs.InBandSell)
	defer os.Remove(graphs.TotalBuy)
	defer os.Remove(graphs.TotalSell)
	defer os.Remove(graphs.Prices)

	if err := generateQtyGraph(&report, graphs.InBandBuy, "In-Bounds Buy Depth", func(tp *TimePoint) uint64 { return tp.BuyQty }); err != nil {
		return fmt.Errorf("failed to generate in-bounds buy graph: %w", err)
	}
	if err := generateQtyGraph(&report, graphs.InBandSell, "In-Bounds Sell Depth", func(tp *TimePoint) uint64 { return tp.SellQty }); err != nil {
		return fmt.Errorf("failed to generate in-bounds sell graph: %w", err)
	}
	if err := generateQtyGraph(&report, graphs.TotalBuy, "Total Buy Depth", func(tp *TimePoint) uint64 { return tp.TotalBuyQty }); err != nil {
		return fmt.Errorf("failed to generate total buy graph: %w", err)
	}
	if err := generateQtyGraph(&report, graphs.TotalSell, "Total Sell Depth", func(tp *TimePoint) uint64 { return tp.TotalSellQty }); err != nil {
		return fmt.Errorf("failed to generate total sell graph: %w", err)
	}
	if err := generatePriceGraph(&report, graphs.Prices); err != nil {
		return fmt.Errorf("failed to generate prices graph: %w", err)
	}

	// Generate PDF
	if err := generatePDFReport(&report, graphs, outputFile); err != nil {
		return fmt.Errorf("failed to generate PDF: %w", err)
	}

	fmt.Printf("Report saved to %s\n", outputFile)
	return nil
}

func generateQtyGraph(report *CoverageReport, outputFile, title string, qty func(tp *TimePoint) uint64) error {
	convFactor := float64(report.ConversionFactor)
	startEpoch := report.TimeSeries[0].EpochNum
	msPerHour := float64(1000 * 60 * 60)

	// Split into contiguous zero/non-zero segments so we don't connect across
	// transitions. Zero segments are drawn along y=0.
	type seg struct {
		isZero bool
		pts    plotter.XYs
	}
	segs := make([]seg, 0, 8)
	var cur seg
	curInit := false
	flush := func() {
		if curInit && len(cur.pts) >= 2 {
			segs = append(segs, cur)
		}
		curInit = false
		cur = seg{}
	}
	for _, pt := range report.TimeSeries {
		yAtomic := qty(pt)
		isZero := yAtomic == 0
		x := float64((pt.EpochNum-startEpoch)*report.EpochDurMs) / msPerHour
		y := 0.0
		if !isZero {
			y = float64(yAtomic) / convFactor
		}
		if !curInit {
			curInit = true
			cur.isZero = isZero
		} else if cur.isZero != isZero {
			flush()
			curInit = true
			cur.isZero = isZero
		}
		cur.pts = append(cur.pts, plotter.XY{X: x, Y: y})
	}
	flush()

	p := plot.New()
	startTime := time.UnixMilli(int64(report.StartTimeMs)).UTC()
	p.Title.Text = title
	p.X.Label.Text = fmt.Sprintf("Hours from %s", startTime.Format("2006-01-02 15:04 UTC"))
	p.Y.Label.Text = fmt.Sprintf("Quantity (%s)", report.BaseAssetSymbol)
	p.Legend.Top = true

	addLine := func(pts plotter.XYs, name string, c color.RGBA, dashed bool) error {
		line, err := plotter.NewLine(pts)
		if err != nil {
			return err
		}
		line.Color, line.Width = c, vg.Points(2)
		if dashed {
			line.Width, line.Dashes = vg.Points(1), []vg.Length{vg.Points(5), vg.Points(5)}
		}
		p.Add(line)
		p.Legend.Add(name, line)
		return nil
	}

	blue := color.RGBA{R: 0, G: 80, B: 200, A: 255}
	// Draw non-zero segments in blue, and zero segments in a lighter gray.
	addedDepthLegend := false
	addedZeroLegend := false
	for _, s := range segs {
		if s.isZero {
			line, err := plotter.NewLine(s.pts)
			if err != nil {
				return err
			}
			line.Color, line.Width = color.RGBA{R: 150, G: 150, B: 150, A: 255}, vg.Points(2)
			p.Add(line)
			if !addedZeroLegend {
				p.Legend.Add("Zero", line)
				addedZeroLegend = true
			}
			continue
		}
		line, err := plotter.NewLine(s.pts)
		if err != nil {
			return err
		}
		line.Color, line.Width = blue, vg.Points(2)
		p.Add(line)
		if !addedDepthLegend {
			p.Legend.Add("Depth", line)
			addedDepthLegend = true
		}
	}

	// Required line uses conventional units already.
	xMin := float64(0)
	xMax := float64(0)
	if len(report.TimeSeries) > 0 {
		xMin = float64((report.TimeSeries[0].EpochNum-startEpoch)*report.EpochDurMs) / msPerHour
		xMax = float64((report.TimeSeries[len(report.TimeSeries)-1].EpochNum-startEpoch)*report.EpochDurMs) / msPerHour
	}
	if err := addLine(plotter.XYs{{X: xMin, Y: report.RequiredQty}, {X: xMax, Y: report.RequiredQty}}, "Required", color.RGBA{R: 90, G: 90, B: 90, A: 255}, true); err != nil {
		return err
	}

	return p.Save(10*vg.Inch, 4*vg.Inch, outputFile)
}

func generatePriceGraph(report *CoverageReport, outputFile string) error {
	startEpoch := report.TimeSeries[0].EpochNum
	msPerHour := float64(1000 * 60 * 60)

	// Split into contiguous segments so 0 values create gaps rather than lines to 0.
	splitSegments := func(get func(*TimePoint) uint64) []plotter.XYs {
		segs := make([]plotter.XYs, 0, 8)
		var cur plotter.XYs
		flush := func() {
			if len(cur) >= 2 {
				segs = append(segs, cur)
			}
			cur = nil
		}
		for _, pt := range report.TimeSeries {
			v := get(pt)
			if v == 0 {
				flush()
				continue
			}
			x := float64((pt.EpochNum-startEpoch)*report.EpochDurMs) / msPerHour
			cur = append(cur, plotter.XY{X: x, Y: float64(v)})
		}
		flush()
		return segs
	}

	bidSegs := splitSegments(func(tp *TimePoint) uint64 { return tp.BestBidRate })
	midSegs := splitSegments(func(tp *TimePoint) uint64 { return tp.MidRate })
	askSegs := splitSegments(func(tp *TimePoint) uint64 { return tp.BestAskRate })

	p := plot.New()
	startTime := time.UnixMilli(int64(report.StartTimeMs)).UTC()
	p.Title.Text = "Mid (midgap), Best Buy, Best Sell"
	p.X.Label.Text = fmt.Sprintf("Hours from %s", startTime.Format("2006-01-02 15:04 UTC"))
	p.Y.Label.Text = "Rate (atomic)"
	p.Legend.Top = true

	addLines := func(segs []plotter.XYs, name string, c color.RGBA) error {
		addedLegend := false
		for _, pts := range segs {
			line, err := plotter.NewLine(pts)
			if err != nil {
				return err
			}
			line.Color, line.Width = c, vg.Points(2)
			p.Add(line)
			if !addedLegend {
				p.Legend.Add(name, line)
				addedLegend = true
			}
		}
		return nil
	}

	if err := addLines(bidSegs, "Best Buy", color.RGBA{R: 0, G: 150, B: 0, A: 255}); err != nil {
		return err
	}
	if err := addLines(midSegs, "Mid (midgap)", color.RGBA{R: 0, G: 80, B: 200, A: 255}); err != nil {
		return err
	}
	if err := addLines(askSegs, "Best Sell", color.RGBA{R: 200, G: 0, B: 0, A: 255}); err != nil {
		return err
	}

	return p.Save(10*vg.Inch, 4*vg.Inch, outputFile)
}

func generatePDFReport(report *CoverageReport, graphs reportGraphs, outputFile string) error {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(20, 20, 20)
	pdf.AddPage()
	pageW, pageH := pdf.GetPageSize()
	left, top, right, bottom := pdf.GetMargins()
	_ = top
	contentW := pageW - left - right
	contentBottom := pageH - bottom
	// Our plots are generated at 10in x 4in => aspect ratio height/width = 0.4.
	graphW := contentW
	graphH := graphW * 0.4

	// Title
	startTime := time.UnixMilli(int64(report.StartTimeMs)).UTC()
	endTime := time.UnixMilli(int64(report.EndTimeMs)).UTC()
	pdf.SetFont("Helvetica", "B", 18)
	pdf.CellFormat(0, 10, "Market Making Coverage Report", "", 1, "C", false, 0, "")
	pdf.SetFont("Helvetica", "", 11)
	pdf.SetTextColor(100, 100, 100)
	pdf.CellFormat(0, 6, fmt.Sprintf("%s to %s", startTime.Format("2006-01-02 15:04 UTC"), endTime.Format("2006-01-02 15:04 UTC")), "", 1, "C", false, 0, "")
	pdf.Ln(8)

	// Summary section
	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "B", 14)
	pdf.CellFormat(0, 8, "Coverage Summary", "", 1, "L", false, 0, "")

	pct := func(covered, total uint64) string {
		if total == 0 {
			return "0.0%"
		}
		return fmt.Sprintf("%.1f%%", float64(covered)/float64(total)*100)
	}

	summaryData := [][]string{
		{"Market", fmt.Sprintf("%s/%s (%d/%d)", report.BaseAssetSymbol, report.QuoteAssetSymbol, report.BaseAssetID, report.QuoteAssetID)},
		{"Account ID", report.AccountID},
		{"Proof Valid", fmt.Sprintf("%t", report.ProofValid)},
		{"Total Epochs", fmt.Sprintf("%d", report.TotalEpochs)},
		{"Buy Coverage", fmt.Sprintf("%d / %d (%s)", report.BuyCoveredEpochs, report.TotalEpochs, pct(report.BuyCoveredEpochs, report.TotalEpochs))},
		{"Sell Coverage", fmt.Sprintf("%d / %d (%s)", report.SellCoveredEpochs, report.TotalEpochs, pct(report.SellCoveredEpochs, report.TotalEpochs))},
		{"Required Qty (both sides)", fmt.Sprintf("%.4f %s", report.RequiredQty, report.BaseAssetSymbol)},
	}
	if report.MaxSpreadPct > 0 {
		summaryData = append(summaryData, []string{"Max Spread", fmt.Sprintf("%.2f%%", report.MaxSpreadPct)})
	}

	for _, row := range summaryData {
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetTextColor(80, 80, 80)
		pdf.CellFormat(50, 6, row[0], "", 0, "L", false, 0, "")
		pdf.SetTextColor(0, 0, 0)
		pdf.CellFormat(0, 6, row[1], "", 1, "L", false, 0, "")
	}

	// Graphs: pack as many as will fit per page.
	type g struct {
		title string
		path  string
	}
	graphList := []g{
		{"In-Bounds Buy Depth", graphs.InBandBuy},
		{"In-Bounds Sell Depth", graphs.InBandSell},
		{"Total Buy Depth", graphs.TotalBuy},
		{"Total Sell Depth", graphs.TotalSell},
		{"Mid (midgap), Best Buy, Best Sell", graphs.Prices},
	}

	imgOpts := fpdf.ImageOptions{ImageType: "PNG", ReadDpi: true}
	addGraph := func(title, path string) {
		// Title(8) + spacing(2) + image(graphH) + spacing(8)
		needed := 8.0 + 2.0 + graphH + 8.0
		if pdf.GetY()+needed > contentBottom {
			pdf.AddPage()
		} else {
			pdf.Ln(8)
		}
		pdf.SetTextColor(0, 0, 0)
		pdf.SetFont("Helvetica", "B", 14)
		pdf.CellFormat(0, 8, title, "", 1, "L", false, 0, "")
		pdf.Ln(2)
		pdf.ImageOptions(path, left, pdf.GetY(), graphW, graphH, false, imgOpts, 0, "")
		// Move cursor below image.
		pdf.SetY(pdf.GetY() + graphH)
	}

	for _, gr := range graphList {
		addGraph(gr.title, gr.path)
	}

	// Unclosed orders section (if any)
	if len(report.UnclosedOrders) > 0 {
		// Start on a fresh page if it won't fit.
		if pdf.GetY()+20 > contentBottom {
			pdf.AddPage()
		} else {
			pdf.Ln(8)
		}
		pdf.SetFont("Helvetica", "B", 14)
		pdf.CellFormat(0, 8, "Unclosed Orders", "", 1, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, 5, "Orders that were not fully matched, cancelled, or revoked:", "", 1, "L", false, 0, "")
		pdf.Ln(2)

		pdf.SetTextColor(0, 0, 0)
		pdf.SetFont("Courier", "", 8)
		for _, orderID := range report.UnclosedOrders {
			pdf.CellFormat(0, 5, orderID.String(), "", 1, "L", false, 0, "")
		}
	}

	return pdf.OutputFileAndClose(outputFile)
}
