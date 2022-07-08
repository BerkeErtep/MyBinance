package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/fatih/color"
	"github.com/gosuri/uilive"
)

var client = futures.NewClient("", "")

func handleError(err error) {
	panic(err)
}
func AppendFilef(fileName string, format string, args ...any) {
	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	if _, err = fmt.Fprintln(f, fmt.Sprintf(format, args...)); err != nil {
		panic(err)
	}
}

func main() {
	writer := uilive.New()

	symbols, err := fetchPerpetualSymbols()
	if err != nil {
		log.Fatalf("fetching perpetual symbols: %v", err)
	}

	var quoteVolumes24hMu sync.RWMutex
	quoteVolumes24h, err := fetchQuoteVolumes(symbols)
	if err != nil {
		log.Fatalf("fetching quote volumes: %v", err)
	}

	var wg sync.WaitGroup

	{
		handleEvent := func(event futures.WsAllMiniMarketTickerEvent) {
			for _, e := range event {
				quoteVolumes24hMu.RLock()
				prev, ok := quoteVolumes24h[e.Symbol]
				quoteVolumes24hMu.RUnlock()
				if !ok {
					continue
				}

				volume, _ := strconv.ParseFloat(e.QuoteVolume, 64)
				if prev != volume {
					quoteVolumes24hMu.Lock()
					quoteVolumes24h[e.Symbol] = volume
					quoteVolumes24hMu.Unlock()
				}
			}
		}

		wg.Add(1)
		doneC, _, err := futures.WsAllMiniMarketTickerServe(handleEvent, handleError)
		if err != nil {
			log.Fatalf("all ticker ws: %v", err)
		}
		go func() {
			<-doneC
			wg.Done()
		}()
	}

	deltaString := func(f float64) string {
		if f >= 0 {
			return fmt.Sprintf("+%.2f%%", f)
		} else {
			return fmt.Sprintf("%.2f%%", f)
		}
	}

	deltaColorString := func(f float64) string {
		s := deltaString(f)
		if f == 0 {
			return color.WhiteString(s)
		} else if f > 0 {
			return color.GreenString(s)
		} else {
			return color.RedString(s)
		}
	}
	{
		intervals := make(map[string]string, len(symbols))
		for _, symbol := range symbols {
			intervals[symbol] = "1m"
		}

		type ping struct {
			ratioThresh float64
			symbol      string
			time        int64
		}

		pings := make(map[ping]struct{})

		type trade struct {
			symbol    string
			time      int64
			ping      ping
			buyPrice  float64
			price     float64
			stopPrice float64
			sellPrice float64
		}

		trades := make(map[string][]trade)

		tw := tabwriter.NewWriter(writer, 0, 2, 2, ' ', 0)

		refreshDisplay := func() {
			var tradeSlice []trade
			for _, t := range trades {
				tradeSlice = append(tradeSlice, t...)
			}
			if len(tradeSlice) > 50 {
				tradeSlice = tradeSlice[:50]
			}

			sort.Slice(tradeSlice, func(i, j int) bool {
				if tradeSlice[i].time == tradeSlice[j].time {
					return tradeSlice[i].symbol < tradeSlice[j].symbol
				}
				return tradeSlice[i].time < tradeSlice[j].time
			})

			for _, t := range tradeSlice {
				priceChange := (t.price/t.buyPrice - 1) * 100
				fmt.Fprintf(tw, "%s\t%.4f\t%s\tBuy: %.4f\tStop: %.4f\tSell: %.4f\tRatio: %.2f\n", t.symbol, t.price, deltaColorString(priceChange), t.buyPrice, t.stopPrice, t.sellPrice, t.ping.ratioThresh)
			}

			tw.Flush()
			writer.Flush()
		}

		// The ratio thresholds to trigger signals for.
		thresholds := []float64{0.75}
		// The price offset percentage to calculate stop and sell price for signals.
		priceOffsetPercent := 1.0

		sort.Sort(sort.Reverse(sort.Float64Slice(thresholds)))

		red := color.New(color.FgRed).FprintfFunc()
		green := color.New(color.FgGreen).FprintfFunc()

		handleEvent := func(event *futures.WsKlineEvent) {
			quoteVolumes24hMu.RLock()
			vol24h, ok := quoteVolumes24h[event.Symbol]
			quoteVolumes24hMu.RUnlock()
			if !ok {
				return
			}

			quoteVolume, err := strconv.ParseFloat(event.Kline.QuoteVolume, 64)
			if err != nil {
				log.Fatalf("parsing quote volume for '%s': %v", event.Symbol, err)
			}

			price, err := strconv.ParseFloat(event.Kline.Close, 64)
			if err != nil {
				log.Fatalf("parsing closing price for '%s': %v", event.Symbol, err)
			}

			ratio := quoteVolume / vol24h * 100
			for _, threshold := range thresholds {
				if ratio > threshold {
					p := ping{
						ratioThresh: threshold,
						symbol:      event.Symbol,
						time:        event.Kline.StartTime,
					}

					if _, ok := pings[p]; ok {
						break // we've already encountered this ping, and any other potential pings after this one
					}

					// Create a trade
					t := trade{
						symbol:    event.Symbol,
						time:      event.Time,
						ping:      p,
						price:     price,
						buyPrice:  price,
						stopPrice: price * (0.999),
						sellPrice: price * (1 + priceOffsetPercent/100),
					}
					trades[event.Symbol] = append(trades[event.Symbol], t)
					refreshDisplay()
					pings[p] = struct{}{}
					break
				}
			}

			// Process active trades. Looping backwards because we might want to remove elements from the slice.
			for i := len(trades[event.Symbol]) - 1; i >= 0; i-- {
				t := trades[event.Symbol][i]
				trades[event.Symbol][i].price = price
				ratioType := "> " + fmt.Sprintf("%.1f", t.ping.ratioThresh)
				if price <= t.stopPrice {
					// Trade is done. Remove it from the slice
					trades[event.Symbol] = append(trades[event.Symbol][:i], trades[event.Symbol][i+1:]...)
					AppendFilef("fail.txt",
						"%s -  %s %s Failed",
						time.UnixMilli(event.Time).Format("2006-01-02 15:04:05"), event.Symbol, ratioType,
					)
					red(writer.Bypass(), "Bought %s at %.4f, sold at %.4f - Fail (%s), Ratio: %0.2f\n", event.Symbol, t.buyPrice, price, deltaString((price/t.buyPrice-1)*100), t.ping.ratioThresh)

				}
				if price >= t.sellPrice {
					// Trade is done. Remove it from the slice
					trades[event.Symbol] = append(trades[event.Symbol][:i], trades[event.Symbol][i+1:]...)
					AppendFilef("success.txt",
						"%s - Symbol: %s, Price: %.8f, Ratio %s Succeed",
						time.UnixMilli(event.Time).Format("2006-01-02 15:04:05"), event.Symbol, price, ratioType,
					)
					green(writer.Bypass(), "Bought %s at %.4f, sold at %.4f - Success (%s), Ratio: %0.2f\n", event.Symbol, t.buyPrice, price, deltaString((price/t.buyPrice-1)*100), t.ping.ratioThresh)
				}
				refreshDisplay()
			}
		}

		wg.Add(1)
		doneC, _, err := futures.WsCombinedKlineServe(intervals, handleEvent, handleError)
		if err != nil {
			log.Fatalf("kline ws: %v", err)
		}
		go func() {
			<-doneC
			wg.Done()
		}()
	}

	wg.Wait()
}

func fetchPerpetualSymbols() (symbols []string, err error) {
	info, err := client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	for _, sym := range info.Symbols {
		if sym.ContractType == futures.ContractTypePerpetual {
			symbols = append(symbols, sym.Symbol)
		}
	}
	return symbols, nil
}

func fetchQuoteVolumes(symbols []string) (quoteVolumes map[string]float64, err error) {
	statss, err := client.NewListPriceChangeStatsService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	// Set the symbols in the map first so that we can use it as a lookup to check whether we should
	// set the quote volume in the map.
	quoteVolumes = make(map[string]float64)
	for _, symbol := range symbols {
		quoteVolumes[symbol] = 0
	}

	for _, stats := range statss {
		if _, ok := quoteVolumes[stats.Symbol]; ok {
			qv, err := strconv.ParseFloat(stats.QuoteVolume, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing quote volume for '%s': %w", stats.Symbol, err)
			}
			quoteVolumes[stats.Symbol] = qv
		}
	}

	return quoteVolumes, nil
}
