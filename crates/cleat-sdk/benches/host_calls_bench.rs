use criterion::{black_box, criterion_group, criterion_main, Criterion};
use serde::{Deserialize, Serialize};

// ── Benchmark types ──

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SmallPayload {
    id: String,
    amount: u64,
    currency: String,
}

#[derive(Debug, Serialize, Deserialize)]
struct MediumPayload {
    workflow_id: String,
    run_id: String,
    status: String,
    input: serde_json::Value,
    history: Vec<EventRecord>,
}

#[derive(Debug, Serialize, Deserialize)]
struct EventRecord {
    event_id: i64,
    event_type: String,
    timestamp_ms: i64,
    attributes: serde_json::Value,
}

// ── Benchmark functions ──

fn bench_json_serialize_small(c: &mut Criterion) {
    let payload = SmallPayload {
        id: "ch_1234567890".to_string(),
        amount: 5000,
        currency: "USD".to_string(),
    };

    c.bench_function("json/serialize_small", |b| {
        b.iter(|| {
            let json = serde_json::to_string(black_box(&payload)).unwrap();
            black_box(json)
        })
    });
}

fn bench_json_deserialize_small(c: &mut Criterion) {
    let json = r#"{"id":"ch_1234567890","amount":5000,"currency":"USD"}"#;

    c.bench_function("json/deserialize_small", |b| {
        b.iter(|| {
            let payload: SmallPayload = serde_json::from_str(black_box(json)).unwrap();
            black_box(payload)
        })
    });
}

fn bench_json_roundtrip_small(c: &mut Criterion) {
    let payload = SmallPayload {
        id: "ch_1234567890".to_string(),
        amount: 5000,
        currency: "USD".to_string(),
    };

    c.bench_function("json/roundtrip_small", |b| {
        b.iter(|| {
            let json = serde_json::to_string(black_box(&payload)).unwrap();
            let parsed: SmallPayload = serde_json::from_str(black_box(&json)).unwrap();
            black_box(parsed)
        })
    });
}

fn bench_json_serialize_medium(c: &mut Criterion) {
    let payload = MediumPayload {
        workflow_id: "wf_abc123def456".to_string(),
        run_id: "run_xyz789ghi012".to_string(),
        status: "RUNNING".to_string(),
        input: serde_json::json!({"orderId": "ord_1", "items": [{"sku": "a", "qty": 2}, {"sku": "b", "qty": 1}]}),
        history: (0..20)
            .map(|i| EventRecord {
                event_id: i,
                event_type: "ActivityTaskCompleted".to_string(),
                timestamp_ms: 1704067200000 + i * 1000,
                attributes: serde_json::json!({"result": format!("step_{}_done", i)}),
            })
            .collect(),
    };

    c.bench_function("json/serialize_medium", |b| {
        b.iter(|| {
            let json = serde_json::to_string(black_box(&payload)).unwrap();
            black_box(json)
        })
    });
}

fn bench_json_deserialize_medium(c: &mut Criterion) {
    let payload = MediumPayload {
        workflow_id: "wf_abc123def456".to_string(),
        run_id: "run_xyz789ghi012".to_string(),
        status: "RUNNING".to_string(),
        input: serde_json::json!({"orderId": "ord_1", "items": [{"sku": "a", "qty": 2}, {"sku": "b", "qty": 1}]}),
        history: (0..20)
            .map(|i| EventRecord {
                event_id: i,
                event_type: "ActivityTaskCompleted".to_string(),
                timestamp_ms: 1704067200000 + i * 1000,
                attributes: serde_json::json!({"result": format!("step_{}_done", i)}),
            })
            .collect(),
    };
    let json = serde_json::to_string(&payload).unwrap();

    c.bench_function("json/deserialize_medium", |b| {
        b.iter(|| {
            let parsed: MediumPayload = serde_json::from_str(black_box(&json)).unwrap();
            black_box(parsed)
        })
    });
}

fn bench_string_buffer_allocation(c: &mut Criterion) {
    c.bench_function("memory/alloc_out_buf", |b| {
        b.iter(|| {
            let buf: Vec<u8> = vec![0u8; 65536];
            black_box(buf)
        })
    });
}

fn bench_string_conversion_params(c: &mut Criterion) {
    let service = "payment_gateway_v2";
    let operation = "process_charge";
    let request = r#"{"orderId":"ord_12345","amount":5000,"currency":"USD","method":"credit_card","token":"tok_abc123def456"}"#;

    let params = (service, operation, request);

    c.bench_function("host_calls/param_conversion", |b| {
        b.iter(|| {
            let svc_ptr = black_box(params.0).as_ptr();
            let svc_len = black_box(params.0).len() as u32;
            let op_ptr = black_box(params.1).as_ptr();
            let op_len = black_box(params.1).len() as u32;
            let req_ptr = black_box(params.2).as_ptr();
            let req_len = black_box(params.2).len() as u32;
            black_box((svc_ptr, svc_len, op_ptr, op_len, req_ptr, req_len))
        })
    });
}

fn bench_format_cleat_result_ok(c: &mut Criterion) {
    let result: Result<SmallPayload, String> = Ok(SmallPayload {
        id: "ch_123".to_string(),
        amount: 5000,
        currency: "USD".to_string(),
    });

    c.bench_function("host_calls/format_result_ok", |b| {
        b.iter(|| {
            // Simulate format_cleat_result
            let r: Result<SmallPayload, String> = black_box(result.clone());
            let output = match r {
                Ok(val) => serde_json::to_string(&val).unwrap(),
                Err(e) => e,
            };
            black_box(output)
        })
    });
}

fn bench_format_cleat_result_err(c: &mut Criterion) {
    let result: Result<SmallPayload, String> =
        Err("something went wrong during payment processing".to_string());

    c.bench_function("host_calls/format_result_err", |b| {
        b.iter(|| {
            let r: Result<SmallPayload, String> = black_box(result.clone());
            let output = match r {
                Ok(val) => serde_json::to_string(&val).unwrap(),
                Err(e) => e,
            };
            black_box(output)
        })
    });
}

criterion_group!(
    benches,
    bench_json_serialize_small,
    bench_json_deserialize_small,
    bench_json_roundtrip_small,
    bench_json_serialize_medium,
    bench_json_deserialize_medium,
    bench_string_buffer_allocation,
    bench_string_conversion_params,
    bench_format_cleat_result_ok,
    bench_format_cleat_result_err,
);
criterion_main!(benches);
