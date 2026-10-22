//! A moving average over the last N readings, as a HotLoop Flow plugin.
//!
//! The guest keeps its window in its own memory between messages, which is
//! what makes a filter possible without the host giving it any storage. It asks
//! for no host access at all: it computes on the message it is handed and
//! returns.

use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};
use std::cell::RefCell;
use std::collections::VecDeque;

#[derive(Deserialize)]
struct Request {
    msg: Map<String, Value>,
    #[serde(default)]
    config: Map<String, Value>,
}

#[derive(Serialize, Default)]
struct Response {
    #[serde(skip_serializing_if = "Vec::is_empty")]
    send: Vec<Vec<Map<String, Value>>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    status: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
}

thread_local! {
    static WINDOW: RefCell<VecDeque<f64>> = RefCell::new(VecDeque::new());
}

/// The host places the request in the guest's own memory through this, so it
/// never writes over anything the guest believes it owns.
#[no_mangle]
pub extern "C" fn hotloop_flow_alloc(size: u32) -> u32 {
    let buf = vec![0u8; size as usize].into_boxed_slice();
    Box::leak(buf).as_mut_ptr() as u32
}

/// Takes a buffer back: the request after the call, the response after it was
/// read.
#[no_mangle]
pub extern "C" fn hotloop_flow_free(ptr: u32, size: u32) {
    if ptr == 0 {
        return;
    }
    unsafe {
        drop(Box::from_raw(std::ptr::slice_from_raw_parts_mut(
            ptr as *mut u8,
            size as usize,
        )));
    }
}

#[no_mangle]
pub extern "C" fn hotloop_flow_process(ptr: u32, len: u32) -> u64 {
    let input = unsafe { std::slice::from_raw_parts(ptr as *const u8, len as usize) };
    let response = match serde_json::from_slice::<Request>(input) {
        Ok(req) => process(req),
        Err(e) => Response {
            error: Some(format!("could not read the request: {e}")),
            ..Default::default()
        },
    };
    let out = serde_json::to_vec(&response).unwrap_or_default().into_boxed_slice();
    let out_len = out.len() as u64;
    let out_ptr = Box::leak(out).as_mut_ptr() as u64;
    (out_ptr << 32) | out_len
}

fn process(mut req: Request) -> Response {
    let field = req
        .config
        .get("field")
        .and_then(Value::as_str)
        .filter(|f| !f.is_empty())
        .unwrap_or("payload")
        .to_string();
    let window = req
        .config
        .get("window")
        .and_then(Value::as_f64)
        .map(|w| w as usize)
        .filter(|w| *w >= 1)
        .unwrap_or(5);

    let reading = match req.msg.get(&field).and_then(Value::as_f64) {
        Some(v) => v,
        None => {
            return Response {
                error: Some(format!("msg.{field} is not a number")),
                ..Default::default()
            }
        }
    };

    let (average, held) = WINDOW.with(|w| {
        let mut w = w.borrow_mut();
        w.push_back(reading);
        while w.len() > window {
            w.pop_front();
        }
        (w.iter().sum::<f64>() / w.len() as f64, w.len())
    });

    req.msg.insert(field, json!(average));
    req.msg.insert("samples".to_string(), json!(held));
    Response {
        send: vec![vec![req.msg]],
        status: Some(json!({"fill": "green", "shape": "dot", "text": format!("{average:.2} over {held}")})),
        error: None,
    }
}
