package portfolio

import (
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// French tax on occasional disposals of digital assets (article 150 VH bis
// du CGI, BOI-RPPM-PVBMC-30-20, formulaire 2086). Only disposals for money
// (or goods and services) are taxable; exchanges between digital assets,
// stablecoins included, are not. For each taxable disposal (2086 lines):
//
//	212 portfolio: value of all digital assets held just before it
//	213 price: what was received; 214 fees: fees of the disposal
//	223 acquisition: total price paid in money for every acquisition so far,
//	    minus the fraction of it contained in each earlier disposal
//	224 gain = (213 − 214) − 223 × 213 / 212
//
// and 223 × 213 / 212 is the fraction of capital this disposal uses.

// Threshold: disposals totalling this much or less in a year are exempt.
var Threshold = decimal.NewFromInt(305)

// Disposal is one line of the 2086, in euros.
type Disposal struct {
	Time                                                    time.Time
	Source                                                  string
	Sold                                                    model.Amount
	Price, Fees, Portfolio, Acquisition, CapitalShare, Gain decimal.Decimal
}

// Tax is a year's 2086, with an estimate of the flat tax.
type Tax struct {
	Year      int
	Disposals []Disposal
	// Total of the prices, net gain (or loss) of the year.
	Total, Gain decimal.Decimal
	Exempt      bool
	Tax, Rate   decimal.Decimal
	Notes       []string
}

// Rate is the flat tax on a year's gains: 12.8 % income tax and the social
// charges. The LFSS 2026 raised the CSG on capital income (revenus du
// patrimoine, which these gains are) to 10.6 % from 2025 income: 18.6 %
// instead of 17.2 %. Before 2019 (article 150 UA): 19 % plus social charges.
func Rate(year int) decimal.Decimal {
	switch {
	case year >= 2025:
		return decimal.RequireFromString("0.314")
	case year >= 2019:
		return decimal.RequireFromString("0.30")
	case year == 2018:
		return decimal.RequireFromString("0.362")
	default:
		return decimal.RequireFromString("0.345")
	}
}

func cents(d decimal.Decimal) decimal.Decimal { return d.Round(2) }

// TaxFR computes year's 2086 from the whole history (in maps a deposit's id
// to the withdrawal it came from), with price in EUR. income: rewards,
// airdrops and coins deposited from outside were taxed as income, so their
// value at reception is part of the acquisition price; otherwise they
// count for 0.
func TaxFR(acts []model.Activity, in map[string]string, price Price, year int, income bool) Tax {
	w := newWalker(Cached(price), in, crypto)
	t := Tax{Year: year, Disposals: []Disposal{}, Rate: Rate(year)}
	acq := decimal.Zero
	end := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	raised := false
	for _, a := range model.ReplayOrder(acts, in) {
		if !a.Time.Before(end) {
			break
		}
		fees := func() decimal.Decimal {
			if a.Fee == nil {
				return decimal.Zero
			}
			return w.worth(*a.Fee, a.Time)
		}
		switch {
		case isTrade(a):
			got, given := legs(a)
			switch {
			case given.Asset.Kind == model.Fiat && crypto(got.Asset):
				acq = acq.Add(w.worth(given, a.Time))
				if a.Fee != nil && a.Fee.Asset.Kind == model.Fiat {
					acq = acq.Add(w.worth(*a.Fee, a.Time))
				}
			case crypto(given.Asset) && got.Asset.Kind == model.Fiat:
				d := Disposal{Time: a.Time, Source: a.SourceID, Sold: given, Acquisition: acq,
					Price: cents(w.worth(got, a.Time)), Fees: cents(fees()), Portfolio: cents(w.value(a.Time))}
				if d.Portfolio.LessThan(d.Price) {
					// Daily closes can value what is held below what it fetched.
					d.Portfolio, raised = d.Price, raised || a.Time.Year() == year
				}
				if d.Portfolio.IsPositive() {
					d.CapitalShare = cents(acq.Mul(d.Price).Div(d.Portfolio))
				}
				d.Gain = d.Price.Sub(d.Fees).Sub(d.CapitalShare)
				acq = acq.Sub(d.CapitalShare)
				if a.Time.Year() == year {
					t.Disposals = append(t.Disposals, d)
					t.Total, t.Gain = t.Total.Add(d.Price), t.Gain.Add(d.Gain)
				}
			}
		case a.Inflow() && crypto(a.Base.Asset) && income && (a.Type != model.Deposit || w.in[a.ID] == ""):
			acq = acq.Add(w.worth(a.Base, a.Time))
		}
		w.apply(a)
	}
	t.Exempt = t.Total.LessThanOrEqual(Threshold)
	if !t.Exempt && t.Gain.IsPositive() {
		t.Tax = cents(t.Gain.Mul(t.Rate))
	}

	t.Notes = append(t.Notes, "Disposals totalling 305 € or less in the year are exempt.",
		"Values use each day's closing price, not the exact time of each operation.")
	if income {
		t.Notes = append(t.Notes, "Rewards, airdrops and coins deposited from outside your sources count at their value on reception.")
	} else {
		t.Notes = append(t.Notes, "Rewards, airdrops and coins deposited from outside your sources count for 0 in the acquisition price; pass rewards=income if they were taxed as income.")
	}
	switch {
	case year >= 2025:
		t.Notes = append(t.Notes, "Rate 31.4 %: 12.8 % income tax and 18.6 % social charges (CSG at 10.6 % from 2025 income, LFSS 2026).")
	case year >= 2019:
		t.Notes = append(t.Notes, "Rate 30 %: 12.8 % income tax and 17.2 % social charges.")
	default:
		t.Notes = append(t.Notes, "Before 2019 other rules applied (article 150 UA): this follows the 2086 method for reference only.")
	}
	t.Notes = append(t.Notes, "Spending crypto on goods or services counts only when the platform reports it as a sale.")
	if raised {
		t.Notes = append(t.Notes, "When the day's prices value the portfolio below a disposal, it is taken equal to the disposal.")
	}
	if u := w.unpricedList(); len(u) > 0 {
		var names []string
		for _, a := range u {
			names = append(names, a.String())
		}
		t.Notes = append(t.Notes, "No price for "+strings.Join(names, ", ")+": counted at 0.")
	}
	return t
}
