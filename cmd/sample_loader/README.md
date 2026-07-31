### Sample Loader

The sample loader generates samples and sends them to a remote-write endpoint.

#### Usage

Generate and load the sample data from the config file.

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u  http://localhost:4000/v1/prometheus/write\?db\=public --start-date 2025-03-09T18:00:00+08:00 --end-date 2025-03-09T19:00:00+08:00 --interval 30s  --tick-interval 1s
```

Remote write v1 is the default. To send `io.prometheus.write.v2.Request`:

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u 'http://localhost:4000/v1/prometheus/write?db=public' --remote-write-version v2
```

Use `--duration 60s` to generate live samples for a finite wall-clock duration.
It cannot be combined with `--infinite`. After queued requests drain, the
loader prints request and sample totals, failures, and samples in successful
requests per second.

With HTTP Basic authorization:

```bash
./bin/sample_loader -c ./configs/debug_samples_400 -u http://localhost:4000/v1/prometheus/write\?db\=public --username myuser --password mypassword
```

#### Configs
`./configs/debug_samples` total time series 31,813,594.
`./configs/debug_samples_400` total time series 416,370.
`./configs/debug_samples_800` total time series 809,664.
