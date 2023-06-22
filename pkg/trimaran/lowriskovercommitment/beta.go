/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package lowriskovercommitment

import (
	"bytes"
	"fmt"
	"math"

	gonum "gonum.org/v1/gonum/mathext"
)

// BetaDistribution :
//
//	http://en.wikipedia.org/wiki/Beta_distribution
//	alpha shape parameter (alpha > 0)
//	beta shape parameter (beta > 0)
type BetaDistribution struct {
	alpha        float64
	beta         float64
	isValid      bool
	firstMoment  float64
	secondMoment float64
	thirdMoment  float64
}

// NewBetaDistribution : constructor
func NewBetaDistribution(alpha, beta float64) *BetaDistribution {
	b := new(BetaDistribution)
	b.isValid = checkValidity(alpha, beta)
	if !b.isValid {
		return nil
	}
	b.alpha = alpha
	b.beta = beta
	// set moments
	b.computeMoments()
	return b
}

// checkValidity : check validity of parameters
func checkValidity(alpha, beta float64) bool {
	return alpha > 0 && beta > 0
}

// computeMoments : compute the first three moments of the distribution
func (b *BetaDistribution) computeMoments() bool {
	if !b.isValid {
		return false
	}
	b.firstMoment = b.alpha / (b.alpha + b.beta)
	b.secondMoment = b.firstMoment * (b.alpha + 1) / (b.alpha + b.beta + 1)
	b.thirdMoment = b.secondMoment * (b.alpha + 2) / (b.alpha + b.beta + 2)
	return true
}

// Mean : E[X], the mean of the random variable X.
func (b *BetaDistribution) Mean() float64 {
	return b.firstMoment
}

// Variance :
// V[X], the variance of the random variable X.
// V[X] = E[X^2] - (E[X])^2
func (b *BetaDistribution) Variance() float64 {
	return b.secondMoment - b.firstMoment*b.firstMoment
}

// DistributionFunction :
// Probability distribution function, PDF(x) of random variable X.
// The probability that X <= x.
func (b *BetaDistribution) DistributionFunction(x float64) float64 {
	p := RegularizedIncomplete(x, b.alpha, b.beta)
	if math.IsNaN(p) || p < 0 || p > 1 {
		p = 0
	}
	return p
}

// DensityFunction :
// Probability density function, pdf(x) of random variable X.
// The probability that x < X < x + dx.
func (b *BetaDistribution) DensityFunction(x float64) float64 {
	var betax = math.Pow(x, b.alpha-1.) * math.Pow((1.-x), (b.beta-1.))
	var betac = Complete(b.alpha, b.beta)
	var pdf = betax / betac
	if math.IsNaN(pdf) || pdf < 0 {
		pdf = 0
	}
	return pdf
}

// MatchMoments : Match the first two moments: m1 and m2
func (b *BetaDistribution) MatchMoments(m1, m2 float64) bool {
	variance := m2 - m1*m1
	if m1 < 0 || m1 > 1 || variance < 0 || variance >= m1*(1-m1) {
		return false
	}
	temp := (m1 * (1 - m1) / variance) - 1
	temp = math.Max(temp, math.SmallestNonzeroFloat64)
	b.alpha = m1 * temp
	b.beta = (1 - m1) * temp
	return b.computeMoments()
}

// GetMaxVariance : Maximum variance for a given mean
func GetMaxVariance(m1 float64) float64 {
	if m1 > 0 && m1 < 1 {
		return m1 * (1 - m1)
	}
	return 0
}

// GetAlpha : the alpha parameter
func (b *BetaDistribution) GetAlpha() float64 {
	return b.alpha
}

// GetBeta : the beta parameter
func (b *BetaDistribution) GetBeta() float64 {
	return b.beta
}

// Print : toString() of the distribution
func (b *BetaDistribution) Print() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "BetaDistribution: ")
	fmt.Fprintf(&buf, "alpha = %f; beta = %f; ", b.alpha, b.beta)
	fmt.Fprintf(&buf, "mean = %f; var = %f; ", b.Mean(), b.Variance())
	fmt.Fprintf(&buf, "m1 = %f; m2 = %f; m3 = %f; ", b.firstMoment, b.secondMoment, b.thirdMoment)
	return buf.String()
}

/*
 * Beta related functions
 */

// Complete : complete beta function, B(a,b), a,b > 0
func Complete(a, b float64) float64 {
	return gonum.Beta(a, b)
}

// RegularizedIncomplete : I_x(a,b) = B(x;a,b) / B(a,b), a,b > 0, x in [0,1],
// where B(x;a,b) is the incomplete and B(a,b) is the complete beta function
func RegularizedIncomplete(x, a, b float64) float64 {
	if a <= 0 || b <= 0 || x < 0 || x > 1 {
		return math.NaN()
	}
	switch x {
	case 0:
		return 0
	case 1:
		return 1
	default:
		return gonum.RegIncBeta(a, b, x)
	}
}

// ComputeProbability : The probability that the resource utilization is less than or equal to a given threshold value
func ComputeProbability(mu, sigma, threshold float64) (float64, *BetaDistribution) {
	if mu == 0 || (sigma == 0 && mu <= threshold) {
		return 1, nil
	}
	if sigma == 0 && mu > threshold {
		return 0, nil
	}
	m1 := mu
	m2 := (sigma * sigma) + (mu * mu)
	betaDist := NewBetaDistribution(1, 1)
	if !betaDist.MatchMoments(m1, m2) {
		return 0, nil
	}
	belowLimit := betaDist.DistributionFunction(threshold)
	if math.IsNaN(belowLimit) {
		return 1, betaDist
	}
	return belowLimit, betaDist
}
