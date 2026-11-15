package parcel

// SetSpawn makes the loop start Cycle's workers through spawn, so a test can run them in its own
// order.
func SetSpawn(l *Loop, spawn func(func())) { l.spawn = spawn }
