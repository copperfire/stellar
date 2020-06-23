package plugins

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"

	"github.com/stellar/kelp/api"
	"github.com/stellar/kelp/model"
	"github.com/stellar/kelp/support/postgresdb"
)

const secondsInHour = 60 * 60
const secondsInDay = 24 * secondsInHour
const timeFormat = time.RFC3339

// sellTwapLevelProvider provides a fixed number of levels using a static percentage spread
type sellTwapLevelProvider struct {
	startPf                                               api.PriceFeed
	offset                                                rateOffset
	orderConstraints                                      *model.OrderConstraints
	dowFilter                                             [7]volumeFilter
	numHoursToSell                                        int
	parentBucketSizeSeconds                               int
	distributeSurplusOverRemainingIntervalsPercentCeiling float64
	exponentialSmoothingFactor                            float64
	minChildOrderSizePercentOfParent                      float64
	random                                                *rand.Rand

	// uninitialized
	activeBucket    *bucketInfo
	previousRoundID *roundID
}

// ensure it implements the LevelProvider interface
var _ api.LevelProvider = &sellTwapLevelProvider{}

// makeSellTwapLevelProvider is a factory method
func makeSellTwapLevelProvider(
	startPf api.PriceFeed,
	offset rateOffset,
	orderConstraints *model.OrderConstraints,
	dowFilter [7]volumeFilter,
	numHoursToSell int,
	parentBucketSizeSeconds int,
	distributeSurplusOverRemainingIntervalsPercentCeiling float64,
	exponentialSmoothingFactor float64,
	minChildOrderSizePercentOfParent float64,
	randSeed int64,
) (api.LevelProvider, error) {
	if numHoursToSell <= 0 || numHoursToSell > 24 {
		return nil, fmt.Errorf("invalid number of hours to sell, expected 0 < numHoursToSell <= 24; was %d", numHoursToSell)
	}

	if parentBucketSizeSeconds <= 0 || parentBucketSizeSeconds > secondsInDay {
		return nil, fmt.Errorf("invalid value for parentBucketSizeSeconds, expected 0 < parentBucketSizeSeconds <= %d (secondsInDay); was %d", secondsInDay, parentBucketSizeSeconds)
	}

	if (secondsInDay % parentBucketSizeSeconds) != 0 {
		return nil, fmt.Errorf("parentBucketSizeSeconds needs to perfectly divide secondsInDay but it does not; secondsInDay is %d and parentBucketSizeSeconds was %d", secondsInDay, parentBucketSizeSeconds)
	}

	if distributeSurplusOverRemainingIntervalsPercentCeiling < 0.0 || distributeSurplusOverRemainingIntervalsPercentCeiling > 1.0 {
		return nil, fmt.Errorf("distributeSurplusOverRemainingIntervalsPercentCeiling is invalid, expected 0.0 <= distributeSurplusOverRemainingIntervalsPercentCeiling <= 1.0; was %.f", distributeSurplusOverRemainingIntervalsPercentCeiling)
	}

	if exponentialSmoothingFactor < 0.0 || exponentialSmoothingFactor > 1.0 {
		return nil, fmt.Errorf("exponentialSmoothingFactor is invalid, expected 0.0 <= exponentialSmoothingFactor <= 1.0; was %.f", exponentialSmoothingFactor)
	}

	if minChildOrderSizePercentOfParent < 0.0 || minChildOrderSizePercentOfParent > 1.0 {
		return nil, fmt.Errorf("minChildOrderSizePercentOfParent is invalid, expected 0.0 <= minChildOrderSizePercentOfParent <= 1.0; was %.f", exponentialSmoothingFactor)
	}

	for i, f := range dowFilter {
		if !f.isSellingBase() {
			return nil, fmt.Errorf("volume filter at index %d was not selling the base asset as expected: %s", i, f.configValue)
		}
	}

	random := rand.New(rand.NewSource(randSeed))
	return &sellTwapLevelProvider{
		startPf:                 startPf,
		offset:                  offset,
		orderConstraints:        orderConstraints,
		dowFilter:               dowFilter,
		numHoursToSell:          numHoursToSell,
		parentBucketSizeSeconds: parentBucketSizeSeconds,
		distributeSurplusOverRemainingIntervalsPercentCeiling: distributeSurplusOverRemainingIntervalsPercentCeiling,
		exponentialSmoothingFactor:                            exponentialSmoothingFactor,
		minChildOrderSizePercentOfParent:                      minChildOrderSizePercentOfParent,
		random:                                                random,
	}, nil
}

type bucketID int64

type dynamicBucketValues struct {
	isNew       bool
	roundID     roundID
	dayBaseSold float64
	baseSold    float64
}

type bucketInfo struct {
	ID                   bucketID
	startTime            time.Time
	endTime              time.Time
	totalBuckets         int64
	totalBucketsTargeted int64
	dayBaseSoldStart     float64
	dayBaseCapacity      float64
	baseSurplusIncluded  float64
	baseCapacity         float64
	minOrderSizeBase     float64
	dynamicValues        *dynamicBucketValues
}

func (b *bucketInfo) dayBaseRemaining() float64 {
	return b.dayBaseCapacity - b.dynamicValues.dayBaseSold
}

func (b *bucketInfo) baseRemaining() float64 {
	return b.baseCapacity - b.dynamicValues.baseSold
}

// String is the Stringer method
func (b *bucketInfo) String() string {
	return fmt.Sprintf("BucketInfo[bucketID=%d, startTime=%s, endTime=%s, totalBuckets=%d, totalBucketsTargeted=%d, dayBaseSoldStart=%.8f, dayBaseCapacity=%.8f, baseSurplusIncluded=%.8f, baseCapacity=%.8f, minOrderSizeBase=%.8f, DynamicBucketValues[isNew=%v, roundID=%d, dayBaseSold=%.8f, dayBaseRemaining=%.8f, baseSold=%.8f, baseRemaining=%.8f, bucketProgress=%.2f%%]]",
		b.ID,
		b.startTime.Format(timeFormat),
		b.endTime.Format(timeFormat),
		b.totalBuckets,
		b.totalBucketsTargeted,
		b.dayBaseSoldStart,
		b.dayBaseCapacity,
		b.baseSurplusIncluded,
		b.baseCapacity,
		b.minOrderSizeBase,
		b.dynamicValues.isNew,
		b.dynamicValues.roundID,
		b.dynamicValues.dayBaseSold,
		b.dayBaseRemaining(),
		b.dynamicValues.baseSold,
		b.baseRemaining(),
		100.0*b.dynamicValues.baseSold/b.baseCapacity,
	)
}

type roundID uint64

type roundInfo struct {
	ID                  roundID
	bucketID            bucketID
	now                 time.Time
	secondsElapsedToday int64
	sizeBaseCapped      float64
	price               float64
}

// String is the Stringer method
func (r *roundInfo) String() string {
	return fmt.Sprintf(
		"RoundInfo[roundID=%d, bucketID=%d, now=%s (day=%s, secondsElapsedToday=%d), sizeBaseCapped=%.8f, price=%.8f]",
		r.ID,
		r.bucketID,
		r.now.Format(timeFormat),
		r.now.Weekday().String(),
		r.secondsElapsedToday,
		r.sizeBaseCapped,
		r.price,
	)
}

// GetLevels impl.
func (p *sellTwapLevelProvider) GetLevels(maxAssetBase float64, maxAssetQuote float64) ([]api.Level, error) {
	now := time.Now().UTC()
	log.Printf("GetLevels, unix timestamp for 'now' in UTC = %d (%s)\n", now.Unix(), now)

	volFilter := p.dowFilter[now.Weekday()]
	log.Printf("volumeFilter = %s\n", volFilter.String())

	rID := p.makeRoundID()
	bucket, e := p.makeBucketInfo(now, volFilter, rID)
	if e != nil {
		return nil, fmt.Errorf("unable to make bucketInfo: %s", e)
	}
	log.Printf("bucketInfo: %s\n", bucket)

	round, e := p.makeRoundInfo(rID, now, bucket)
	if e != nil {
		return nil, fmt.Errorf("unable to make roundInfo: %s", e)
	}
	log.Printf("roundInfo: %s\n", round)

	// save bucket and round for future rounds
	p.activeBucket = bucket
	p.previousRoundID = &round.ID

	return []api.Level{{
		Price:  *model.NumberFromFloat(round.price, p.orderConstraints.PricePrecision),
		Amount: *model.NumberFromFloat(round.sizeBaseCapped, p.orderConstraints.VolumePrecision),
	}}, nil
}

func (p *sellTwapLevelProvider) makeFirstBucketFrame(
	now time.Time,
	volFilter volumeFilter,
	startTime time.Time,
	secondsElapsedToday int64,
	bID bucketID,
	rID roundID,
) (*bucketInfo, error) {
	endTime := ceilDate(now)
	totalBuckets := int64(math.Ceil(float64(endTime.Unix()-startTime.Unix()) / float64(p.parentBucketSizeSeconds)))
	totalBucketsTargeted := int64(math.Ceil(float64(p.numHoursToSell*secondsInHour) / float64(p.parentBucketSizeSeconds)))

	dayBaseCapacity, e := volFilter.mustGetBaseAssetCapInBaseUnits()
	if e != nil {
		return nil, fmt.Errorf("could not fetch base asset cap in base units: %s", e)
	}

	dailyVolumeValues, e := volFilter.dailyValuesByDate(now.Format(postgresdb.DateFormatString))
	if e != nil {
		return nil, fmt.Errorf("could not fetch daily values for today: %s", e)
	}
	dayBaseSoldStart := dailyVolumeValues.baseVol

	baseSurplus := 0.0
	baseCapacity := float64(dayBaseCapacity) / float64(totalBucketsTargeted)
	minOrderSizeBase := p.minChildOrderSizePercentOfParent * baseCapacity
	// upon instantiation the first bucket frame does not have anything sold beyond the starting values
	dynamicValues := &dynamicBucketValues{
		isNew:       true,
		roundID:     rID,
		dayBaseSold: dayBaseSoldStart,
		baseSold:    0.0,
	}

	return &bucketInfo{
		ID:                   bID,
		startTime:            startTime,
		endTime:              endTime,
		totalBuckets:         totalBuckets,
		totalBucketsTargeted: totalBucketsTargeted,
		dayBaseSoldStart:     dayBaseSoldStart,
		dayBaseCapacity:      dayBaseCapacity,
		baseSurplusIncluded:  baseSurplus,
		baseCapacity:         baseCapacity,
		minOrderSizeBase:     minOrderSizeBase,
		dynamicValues:        dynamicValues,
	}, nil
}

func (p *sellTwapLevelProvider) updateExistingBucket(now time.Time, volFilter volumeFilter, rID roundID) (*bucketInfo, error) {
	bucketCopy := *p.activeBucket
	bucket := &bucketCopy

	dailyVolumeValues, e := volFilter.dailyValuesByDate(now.Format(postgresdb.DateFormatString))
	if e != nil {
		return nil, fmt.Errorf("could not fetch daily values for today: %s", e)
	}
	dayBaseSold := dailyVolumeValues.baseVol

	bucket.dynamicValues = &dynamicBucketValues{
		isNew:       false,
		roundID:     rID,
		dayBaseSold: dayBaseSold,
		baseSold:    dayBaseSold - bucket.dayBaseSoldStart,
	}
	return bucket, nil
}

func (p *sellTwapLevelProvider) cutoverToNewBucket(
	now time.Time,
	volFilter volumeFilter,
	startTime time.Time,
	secondsElapsedToday int64,
	bID bucketID,
	rID roundID,
) (*bucketInfo, error) {
	if bID != p.activeBucket.ID+1 {
		// TODO think about day boundaries!!
		return nil, fmt.Errorf("new bucketID (%d) was not one more than the previous bucketID (%d)", bID, p.activeBucket.ID)
	}

	// start from a new bucket
	bucket, e := p.makeFirstBucketFrame(now, volFilter, startTime, secondsElapsedToday, bID, rID)
	if e != nil {
		return nil, fmt.Errorf("unable to make first bucket frame when cutting over with new bucketID (ID=%d): %s", bID, e)
	}

	// update values that will change from a brand new bucket
	thisBucketDayBaseSoldStart := p.activeBucket.dynamicValues.dayBaseSold
	thisBucketDayBaseSold := bucket.dayBaseSoldStart // pull dayBaseSold from what was queried, this can be more than what was eventually sold in last bucket
	// TODO think about day boundaries!!
	bucket.dayBaseSoldStart = thisBucketDayBaseSoldStart // start new bucket with ending value of previous bucket
	bucket.dynamicValues = &dynamicBucketValues{
		isNew:       true,
		roundID:     rID,
		dayBaseSold: thisBucketDayBaseSold,
		baseSold:    thisBucketDayBaseSold - thisBucketDayBaseSoldStart,
	}

	// the total surplus remaining up until this point gets distributed over the remaining buckets
	averageBaseCapacity := bucket.baseCapacity
	numPreviousBuckets := bID // buckets are 0-indexed, so bID is equal to numbers of previous buckets
	expectedSold := averageBaseCapacity * float64(numPreviousBuckets)
	totalSurplus := expectedSold - thisBucketDayBaseSoldStart
	totalRemainingBuckets := bucket.totalBuckets - int64(numPreviousBuckets)
	bucket.baseSurplusIncluded = p.firstDistributionOfBaseSurplus(totalSurplus, totalRemainingBuckets)
	bucket.baseCapacity = averageBaseCapacity + bucket.baseSurplusIncluded

	return bucket, nil
}

func (p *sellTwapLevelProvider) makeBucketInfo(now time.Time, volFilter volumeFilter, rID roundID) (*bucketInfo, error) {
	startTime := floorDate(now)
	secondsElapsedToday := now.Unix() - startTime.Unix()
	bID := bucketID(secondsElapsedToday / int64(p.parentBucketSizeSeconds))

	if p.activeBucket == nil {
		bucket, e := p.makeFirstBucketFrame(now, volFilter, startTime, secondsElapsedToday, bID, rID)
		if e != nil {
			return nil, fmt.Errorf("could not make first bucket: %s", e)
		}
		return bucket, nil
	}

	if bID == p.activeBucket.ID {
		bucket, e := p.updateExistingBucket(now, volFilter, rID)
		if e != nil {
			return nil, fmt.Errorf("could not update existing bucket (ID=%d): %s", bID, e)
		}
		return bucket, nil
	}

	return p.cutoverToNewBucket(now, volFilter, startTime, secondsElapsedToday, bID, rID)
}

/*
Using a geometric series calculation:
Sn = a * (r^n - 1) / (r - 1)
a = Sn * (r - 1) / (r^n - 1)
a = 8,000 * (0.5 - 1) / (0.5^4 - 1)
a = 8,000 * (-0.5) / (0.0625 - 1)
a = 8,000 * (0.5/0.9375)
a = 4,266.67
*/
func (p *sellTwapLevelProvider) firstDistributionOfBaseSurplus(totalSurplus float64, totalRemainingBuckets int64) float64 {
	Sn := totalSurplus
	r := p.exponentialSmoothingFactor
	n := math.Ceil(p.distributeSurplusOverRemainingIntervalsPercentCeiling * float64(totalRemainingBuckets))

	a := Sn * (r - 1.0) / (math.Pow(r, n) - 1.0)
	return a
}

func (p *sellTwapLevelProvider) makeRoundID() roundID {
	if p.previousRoundID == nil {
		return roundID(0)
	}
	return *p.previousRoundID + 1
}

func (p *sellTwapLevelProvider) makeRoundInfo(rID roundID, now time.Time, bucket *bucketInfo) (*roundInfo, error) {
	secondsElapsedToday := now.Unix() - bucket.startTime.Unix()

	var sizeBaseCapped float64
	if bucket.baseRemaining() <= bucket.minOrderSizeBase {
		sizeBaseCapped = bucket.baseRemaining()
	} else {
		sizeBaseCapped = bucket.minOrderSizeBase + (p.random.Float64() * (bucket.baseRemaining() - bucket.minOrderSizeBase))
	}

	price, e := p.startPf.GetPrice()
	if e != nil {
		return nil, fmt.Errorf("could not get price from feed: %s", e)
	}
	adjustedPrice, wasModified := p.offset.apply(price)
	if wasModified {
		log.Printf("feed price (adjusted): %.8f\n", adjustedPrice)
	}

	return &roundInfo{
		ID:                  rID,
		bucketID:            bucket.ID,
		now:                 now,
		secondsElapsedToday: secondsElapsedToday,
		sizeBaseCapped:      sizeBaseCapped,
		price:               adjustedPrice,
	}, nil
}

// GetFillHandlers impl
func (p *sellTwapLevelProvider) GetFillHandlers() ([]api.FillHandler, error) {
	return nil, nil
}

func floorDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func ceilDate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
}