package cron

import (
	"log"
	"runtime"
	"sort"
	"time"
)

// Cron keeps track of any number of entries, invoking the associated func as
// specified by the schedule. It may be started, stopped, and the entries may
// be inspected while running.
type Cron struct {
	entries  []*Entry
	stop     chan struct{}
	add      chan *Entry
	remove   chan EntryID
	snapshot chan []Entry
	running  bool
	ErrorLog *log.Logger
	location *time.Location
	nextID   EntryID
}

// Job is an interface for submitted cron jobs.
type Job interface {
	Run()
}

// Schedule describes a job's duty cycle.
type Schedule interface {
	// Next returns the next activation time, later than the given time.
	// Next is invoked initially, and then each time the job is run.
	Next(time.Time) time.Time
}

// EntryID identifies an entry within a Cron instance
type EntryID int

// Entry consists of a schedule and the func to execute on that schedule.
type Entry struct {
	// ID is the cron-assigned ID of this entry, which may be used to look up a
	// snapshot or remove it.
	ID EntryID

	// Schedule on which this job should be run.
	Schedule Schedule

	// Next time the job will run, or the zero time if Cron has not been
	// started or this entry's schedule is unsatisfiable
	Next time.Time

	// Prev is the last time this job was run, or the zero time if never.
	Prev time.Time

	// Job is the thing to run when the Schedule is activated.
	Job Job
}

// Valid returns true if this is not the zero entry.
func (e Entry) Valid() bool { return e.ID != 0 }

// byTime is a wrapper for sorting the entry array by time
// (with zero time at the end).
type byTime []*Entry

func (s byTime) Len() int      { return len(s) }
func (s byTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s byTime) Less(i, j int) bool {
	// Two zero times should return false.
	// Otherwise, zero is "greater" than any other time.
	// (To sort it at the end of the list.)
	if s[i].Next.IsZero() {
		return false
	}
	if s[j].Next.IsZero() {
		return true
	}
	return s[i].Next.Before(s[j].Next)
}

// New returns a new Cron job runner, in the Local time zone.
func New() *Cron {
	return NewWithLocation(time.Now().Location())
}

// NewWithLocation returns a new Cron job runner.
func NewWithLocation(location *time.Location) *Cron {
	return &Cron{
		entries:  nil,
		add:      make(chan *Entry),
		stop:     make(chan struct{}),
		snapshot: make(chan []Entry),
		remove:   make(chan EntryID),
		running:  false,
		ErrorLog: nil,
		location: location,
	}
}

// FuncJob is a wrapper that turns a func() into a cron.Job
type FuncJob func()

func (f FuncJob) Run() { f() }

// AddFunc adds a func to the Cron to be run on the given schedule.
func (c *Cron) AddFunc(spec string, cmd func()) (EntryID, error) {
	return c.AddJob(spec, FuncJob(cmd))
}

// AddJob adds a Job to the Cron to be run on the given schedule.
func (c *Cron) AddJob(spec string, cmd Job) (EntryID, error) {
	schedule, err := Parse(spec)
	if err != nil {
		return 0, err
	}
	return c.Schedule(schedule, cmd), nil
}

// Schedule adds a Job to the Cron to be run on the given schedule.
func (c *Cron) Schedule(schedule Schedule, cmd Job) EntryID {
	c.nextID++
	entry := &Entry{
		ID:       c.nextID,
		Schedule: schedule,
		Job:      cmd,
	}
	if !c.running {
		c.entries = append(c.entries, entry)
	} else {
		c.add <- entry
	}
	return entry.ID
}

// Entries returns a snapshot of the cron entries.
func (c *Cron) Entries() []Entry {
	if c.running {
		c.snapshot <- nil
		return <-c.snapshot
	}
	return c.entrySnapshot()
}

// Location gets the time zone location
func (c *Cron) Location() *time.Location {
	return c.location
}

// Entry returns a snapshot of the given entry, or nil if it couldn't be found.
func (c *Cron) Entry(id EntryID) Entry {
	for _, entry := range c.Entries() {
		if id == entry.ID {
			return entry
		}
	}
	return Entry{}
}

// Remove an entry from being run in the future.
func (c *Cron) Remove(id EntryID) {
	if c.running {
		c.remove <- id
	} else {
		c.removeEntry(id)
	}
}

// Start the cron scheduler in its own go-routine.
func (c *Cron) Start() {
	if c.running {
		return
	}
	c.running = true
	go c.run()
}

// Run the cron scheduler, or no-op if already running.
func (c *Cron) Run() {
	if c.running {
		return
	}
	c.running = true
	c.run()
}

func (c *Cron) runWithRecovery(j Job) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			c.logf("cron: panic running job: %v\n%s", r, buf)
		}
	}()
	j.Run()
}

// Run the scheduler.. this is private just due to the need to synchronize
// access to the 'running' state variable.
func (c *Cron) run() {
	// Figure out the next activation times for each entry.
	now := time.Now().In(c.location)
	for _, entry := range c.entries {
		entry.Next = entry.Schedule.Next(now)
	}

	for {
		// Determine the next entry to run.
		sort.Sort(byTime(c.entries))

		var effective time.Time
		if len(c.entries) == 0 || c.entries[0].Next.IsZero() {
			// If there are no entries yet, just sleep - it still handles new entries
			// and stop requests.
			effective = now.AddDate(10, 0, 0)
		} else {
			effective = c.entries[0].Next
		}

		timer := time.NewTimer(effective.Sub(now))
		select {
		case now = <-timer.C:
			now = now.In(c.location)

			// The following two ifs are there to prevent duplicate
			// job invocation if sleep wakes early whilst preserving
			// the behaviour to reschedule from now which is problematic
			// if a machine goes to sleep
			if abs(effective.Sub(now)) < 500 * time.Millisecond {
				// If the time is triggered within 0.5 second either before
				// or after - we accept the effective time as now
				// for the purposes of computing the next run
				now = effective.In(c.location)
			}
			if now.Before(effective) {
				// If now is before the expected time wait some more
				// Sleep for 0.5 seconds to ensure this does not spin in case
				// the sleep keeps occuring
				time.Sleep(500 * time.Millisecond)
				continue
			}

			// Run every entry whose next time was this effective time.
			for _, e := range c.entries {
				if e.Next != effective {
					break
				}
				go c.runWithRecovery(e.Job)
				e.Prev = e.Next
				e.Next = e.Schedule.Next(now)
			}
			continue

		case newEntry := <-c.add:
			c.entries = append(c.entries, newEntry)
			newEntry.Next = newEntry.Schedule.Next(time.Now().In(c.location))

		case <-c.snapshot:
			c.snapshot <- c.entrySnapshot()

		case id := <-c.remove:
			c.removeEntry(id)

		case <-c.stop:
			timer.Stop()
			return
		}

		// 'now' should be updated after newEntry and snapshot cases.
		now = time.Now().In(c.location)
		timer.Stop()
	}
}

func abs(x time.Duration) time.Duration {
	if x > 0 {
		return x
	} else {
		return -x
	}
}

// Logs an error to stderr or to the configured error log
func (c *Cron) logf(format string, args ...interface{}) {
	if c.ErrorLog != nil {
		c.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Stop stops the cron scheduler if it is running; otherwise it does nothing.
func (c *Cron) Stop() {
	if !c.running {
		return
	}
	c.stop <- struct{}{}
	c.running = false
}

// entrySnapshot returns a copy of the current cron entry list.
func (c *Cron) entrySnapshot() []Entry {
	var entries = make([]Entry, len(c.entries))
	for i, e := range c.entries {
		entries[i] = *e
	}
	return entries
}

func (c *Cron) removeEntry(id EntryID) {
	var entries []*Entry
	for _, e := range c.entries {
		if e.ID != id {
			entries = append(entries, e)
		}
	}
	c.entries = entries
}
