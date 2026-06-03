# Satience Benchmark

Benchmark satience SAT solver using gbd-tools and the Global Benchmark Database (GBD).

## Setup

The project is already configured with `uv`. Dependencies are installed via `pyproject.toml`.

## Usage

1. Build the satience solver:
   ```bash
   cd ..
   go build -o satience ./cmd/satience
   ```

2. Run the benchmark:
   ```bash
   ./run-bench.sh
   ```

   Or with custom paths:
   ```bash
   ./run-bench.sh /path/to/satience /path/to/meta.db
   ```

   Or directly with environment variables:
   ```bash
   SATIENCE_SOLVER=/path/to/satience GBD_DB=/path/to/meta.db uv run python benchmark.py
   ```

## Configuration

- `SATIENCE_SOLVER`: Path to the satience binary (default: `../satience`)
- `GBD_DB`: Path to GBD database file (default: `meta.db`)

## Script Options

Edit `benchmark.py` to change:
- `num_instances`: Number of instances to test (default: 10)
- `timeout`: Per-instance timeout in seconds (default: 300)

## Files

- `benchmark.py`: Main benchmark script
- `test_gbd.py`: Test GBD database queries
- `run-bench.sh`: Convenience script to run benchmarks
- `meta.db`: GBD metadata database (downloaded from benchmark-database.de)

## Download GBD Database

If you need to download the GBD database again:

```bash
curl -L -o meta.db https://benchmark-database.de/getdatabase/meta.db
```
