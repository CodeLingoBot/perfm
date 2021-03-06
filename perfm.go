package perfm

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	hist "github.com/arthurkiller/perfm/histogram"
)

// Job give out a job for parallel call
// 1. start workers
// 		1. workers call job.Copy()
// 		2. for-loop do
// 			* job.Pre()
// 			* job.Do()
// 		3. after for-loop call job.After()
// 2. caculate the summary
type Job interface {
	// Copy will copy a job for parallel call
	Copy() (Job, error)
	// Pre will called before do
	Pre() error
	// Do contains the core job here
	Do() error
	// After contains the clean job after job done
	After()
}

//PerfMonitor define the atcion about perfmonitor
type PerfMonitor interface {
	Regist(Job) //regist the job to perfm
	Start()     //start the perf monitor
}

// Regist the job to perfm
func Regist(j Job) {
	p.Regist(j)
}

// Start the perf monitor
func Start() {
	p.Start()
}

type perfmonitor struct {
	Sum   float64 //Sum of the per request cost
	Stdev float64 //Standard Deviation
	Mean  float64 //Mean about distribution
	Total int64   //total request by count

	Config                            //configration for perfm
	done           chan int           //stop the perfm
	startTime      time.Time          //keep the start time
	timer          <-chan time.Time   //the frequency sampling timer
	collector      chan time.Duration //get the request cost from every done()
	errCount       int64              //error counter count error request
	localCount     int                //count for the number in the sampling times
	localTimeCount time.Duration      //count for the sampling time total costs
	buffer         chan int64         //buffer the test time for latter add to the historgam
	histogram      hist.Histogram     //used to print the histogram
	wg             sync.WaitGroup     //wait group to block the stop and sync the work thread

	//job implement benchmark job
	//error occoured in job.Do will be collected
	job Job
}

func New(options ...Options) PerfMonitor { return &perfmonitor{Config: newConfig(options...)} }

// Regist a job into perfmonitor fro benchmark
func (p *perfmonitor) Regist(job Job) {
	p.timer = time.Tick(time.Second * time.Duration(p.Frequency))
	p.collector = make(chan time.Duration, p.BufferSize)
	p.histogram = hist.NewHistogram(p.BinsNumber)
	p.buffer = make(chan int64, 100000000)
	p.done = make(chan int, 0)
	p.wg = sync.WaitGroup{}
	p.job = job

	p.Sum = 0
	p.Mean = 0
	p.Stdev = 0
	p.Total = 0
	p.errCount = 0
	p.localCount = 0
	p.localTimeCount = 0
}

// Start the benchmark with given arguments on regisit
func (p *perfmonitor) Start() {
	if p.job == nil {
		panic("error job does not registered yet")
	}
	var localwg sync.WaitGroup

	// If job implement descripetion as Stringer
	if _, ok := p.job.(fmt.Stringer); ok {
		fmt.Println(p.job)
	}
	fmt.Println("===============================================")

	p.wg.Add(1)
	go func() {
		p.startTime = time.Now()
		var cost time.Duration
		for {
			select {
			case cost = <-p.collector:
				p.localCount++
				p.localTimeCount += cost
				p.buffer <- int64(cost)
			case <-p.timer:
				if p.localCount == 0 {
					continue
				}
				if !p.NoPrint {
					fmt.Printf("%s \t  Qps: %d \t  Avg Latency: %.3fms\n", time.Now().Format("15:04:05.000"),
						p.localCount, float64(p.localTimeCount.Nanoseconds()/int64(p.localCount))/1000000)
				}
				p.localCount = 0
				p.localTimeCount = 0
			case <-p.done:
				localwg.Wait()
				close(p.collector)
				for cost := range p.collector {
					p.localCount++
					p.localTimeCount += cost
					p.buffer <- int64(cost)
				}
				if !p.NoPrint {
					fmt.Printf("%s \t  Qps: %d \t  Avg Latency: %.3fms\n", time.Now().Format("15:04:05.000"),
						p.localCount, float64(p.localTimeCount.Nanoseconds()/int64(p.localCount))/1000000)
				}
				close(p.buffer)
				p.wg.Done()
				return
			}
		}
	}()

	if p.Number > 0 {
		// in total request module
		sum := int64(p.Number)
		for i := 0; i < p.Parallel; i++ {
			localwg.Add(1)
			go func() {
				defer localwg.Done()
				var err error
				job, err := p.job.Copy()
				if err != nil {
					fmt.Println("error in do copy", err)
					return
				}
				defer job.After()
				var start time.Time
				var l int64
				for {
					// check if the request reach the goal
					if l = atomic.AddInt64(&p.Total, 1); l > sum {
						if l == sum+1 { // make sure only close once
							close(p.done)
						}
						return
					}
					if err = job.Pre(); err != nil {
						fmt.Println("error in do pre job", err)
						return
					}
					start = time.Now()
					err = job.Do()
					p.collector <- time.Since(start)
					if err != nil {
						atomic.AddInt64(&p.errCount, 1)
					}
				}
			}()
		}
	} else {
		// in test duration module
		// start all the worker and do job till cancelled
		starter := make(chan struct{})
		for i := 0; i < p.Parallel; i++ {
			localwg.Add(1)
			go func() {
				defer localwg.Done()
				var err error
				job, err := p.job.Copy()
				if err != nil {
					fmt.Println("error in do copy", err)
					return
				}
				defer job.After()
				var start time.Time
				<-starter
				for {
					select {
					case <-p.done:
						return
					default:
						if err = job.Pre(); err != nil {
							fmt.Println("error in do pre job", err)
							return
						}
						start = time.Now()
						err = job.Do()
						p.collector <- time.Since(start)
						if err != nil {
							atomic.AddInt64(&p.errCount, 1)
						}
						atomic.AddInt64(&p.Total, 1)
					}
				}
			}()
		}

		p.wg.Add(1)
		go func() {
			// stoper to cancell all the workers
			p.wg.Done()
			close(starter)
			time.Sleep(time.Second * time.Duration(p.Duration))
			close(p.done)
			return
		}()
	}

	// wait job done and do summarize
	p.wg.Wait()
	var sum2, max, min, p70, p80, p90, p95 float64
	min = math.MaxFloat64
	p.Total--
	sortedSlice := make([]float64, 0, len(p.buffer))
	for d := range p.buffer {
		sortedSlice = append(sortedSlice, float64(d))
		p.histogram.Add(float64(d))
		p.Sum += float64(d)
		sum2 += float64(d * d)
	}
	sort.Slice(sortedSlice, func(i, j int) bool { return sortedSlice[i] < sortedSlice[j] })
	p70 = sortedSlice[int(float64(p.Total)*0.7)] / 1000000
	p80 = sortedSlice[int(float64(p.Total)*0.8)] / 1000000
	p90 = sortedSlice[int(float64(p.Total)*0.9)] / 1000000
	p95 = sortedSlice[int(float64(p.Total)*0.95)] / 1000000
	min = sortedSlice[0]
	max = sortedSlice[p.Total-1]

	p.Mean = p.histogram.(*hist.NumericHistogram).Mean()
	p.Stdev = math.Sqrt((float64(sum2) - 2*float64(p.Mean*p.Sum) +
		float64(float64(p.Total)*p.Mean*p.Mean)) / float64(p.Total))

	fmt.Println("\n===============================================")
	// here show the histogram
	if p.errCount != 0 {
		fmt.Printf("Total errors: %v\t Error percentage: %.3f%%\n", p.errCount,
			float64(p.errCount*100)/float64(p.Total))
	}
	fmt.Printf("MAX: %.3fms MIN: %.3fms MEAN: %.3fms STDEV: %.3f CV: %.3f%% ", max/1000000,
		min/1000000, p.Mean/1000000, p.Stdev/1000000, p.Stdev/float64(p.Mean)*100)
	fmt.Println(p.histogram)
	fmt.Println("===============================================")
	fmt.Printf("Summary:\n70%% in:\t%.3fms\n80%% in:\t%.3fms\n90%% in:\t%.3fms\n95%% in:\t%.3fms\n",
		p70, p80, p90, p95)
}
