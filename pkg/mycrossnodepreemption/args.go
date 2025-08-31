// args.go

package mycrossnodepreemption

var OptimizeCadence = parseCadence(getenv("OPTIMIZE_CADENCE", "for_every")) // Choices: "for_every", "in_batches", "continuously"
var OptimizeAt = parseOptimizeAt(getenv("OPTIMIZE_AT", "postfilter"))       // Choices: "preenqueue", "postfilter" (ignored in continuous mode)
