# Network Traffic Monitor

This tool was built to analyze AWS network throttling and microbursts.
To understand baseline you want to checkout the following resources:

- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-network-bandwidth.html
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/compute-optimized-instances.html#compute-network-performance

Network Traffic Monitor exposes network bytes receive and transmit
counters in second granularity.

We store all data in a ring data structure with 300 buckets for each metric.

To query raw data use /raw endpoint and query `n` with `0 < n < 300`
the number of buckets from now you want to get.

To query aggregated values use /metrics endpoint. Query `n` with `0
< n < 300` the number of buckets we should aggregate. Query `baseline`
to set the value up to we ignore the sum of rx and tx bytes.

```
# metric value
% curl http://localhost:8080/metrics\?baseline\=10000\&n\=10
above_baseline_count 5
above_baseline_area_sum 166319

# index metric value
% curl http://localhost:8080/raw\?n\=2
207 receive_bytes 19313959443
207 transmit_bytes 9011116210
208 receive_bytes 19313965946
208 transmit_bytes 9011125969
```
