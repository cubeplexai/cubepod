//! Minimal JSON Parser & Serializer (Zero-Dependency) and URL Decoding utilities.

use std::collections::HashMap;

#[derive(Debug, Clone, PartialEq)]
pub enum JsonValue {
    Null,
    Bool(bool),
    Number(f64),
    String(String),
    Array(Vec<JsonValue>),
    Object(HashMap<String, JsonValue>),
}

impl JsonValue {
    pub fn as_str(&self) -> Option<&str> {
        match self {
            JsonValue::String(s) => Some(s.as_str()),
            _ => None,
        }
    }

    pub fn as_bool(&self) -> Option<bool> {
        match self {
            JsonValue::Bool(b) => Some(*b),
            _ => None,
        }
    }

    pub fn as_u64(&self) -> Option<u64> {
        match self {
            JsonValue::Number(n) if *n >= 0.0 => Some(*n as u64),
            _ => None,
        }
    }

    pub fn get(&self, key: &str) -> Option<&JsonValue> {
        match self {
            JsonValue::Object(map) => map.get(key),
            _ => None,
        }
    }

    pub fn as_object(&self) -> Option<&HashMap<String, JsonValue>> {
        match self {
            JsonValue::Object(map) => Some(map),
            _ => None,
        }
    }

    pub fn parse(s: &str) -> Result<Self, String> {
        let mut p = JsonParser::new(s);
        let val = p.parse_value()?;
        p.skip_whitespace();
        if p.chars.next().is_some() {
            return Err("trailing characters after json".to_string());
        }
        Ok(val)
    }
}

pub struct JsonParser<'a> {
    pub chars: std::iter::Peekable<std::str::Chars<'a>>,
}

impl<'a> JsonParser<'a> {
    pub fn new(input: &'a str) -> Self {
        Self {
            chars: input.chars().peekable(),
        }
    }

    pub fn skip_whitespace(&mut self) {
        while let Some(&c) = self.chars.peek() {
            if c.is_whitespace() {
                self.chars.next();
            } else {
                break;
            }
        }
    }

    pub fn parse_value(&mut self) -> Result<JsonValue, String> {
        self.skip_whitespace();
        match self.chars.peek() {
            Some('"') => self.parse_string().map(JsonValue::String),
            Some('{') => self.parse_object().map(JsonValue::Object),
            Some('[') => self.parse_array().map(JsonValue::Array),
            Some('t') | Some('f') => self.parse_bool().map(JsonValue::Bool),
            Some('n') => self.parse_null().map(|_| JsonValue::Null),
            Some(&c) if c == '-' || c.is_ascii_digit() => {
                self.parse_number().map(JsonValue::Number)
            }
            Some(c) => Err(format!("unexpected character in json: {c}")),
            None => Err("unexpected end of json input".to_string()),
        }
    }

    fn parse_string(&mut self) -> Result<String, String> {
        self.chars.next(); // consume opening quote
        let mut s = String::new();
        while let Some(c) = self.chars.next() {
            match c {
                '"' => return Ok(s),
                '\\' => match self.chars.next() {
                    Some('"') => s.push('"'),
                    Some('\\') => s.push('\\'),
                    Some('/') => s.push('/'),
                    Some('b') => s.push('\u{08}'),
                    Some('f') => s.push('\u{0C}'),
                    Some('n') => s.push('\n'),
                    Some('r') => s.push('\r'),
                    Some('t') => s.push('\t'),
                    Some('u') => {
                        let mut hex = String::with_capacity(4);
                        for _ in 0..4 {
                            if let Some(h) = self.chars.next() {
                                hex.push(h);
                            } else {
                                return Err("incomplete unicode escape".to_string());
                            }
                        }
                        let code = u32::from_str_radix(&hex, 16)
                            .map_err(|e| format!("invalid hex in unicode escape: {e}"))?;
                        let ch = char::from_u32(code)
                            .ok_or_else(|| "invalid unicode code point".to_string())?;
                        s.push(ch);
                    }
                    Some(other) => s.push(other),
                    None => return Err("unterminated escape sequence".to_string()),
                },
                other => s.push(other),
            }
        }
        Err("unterminated string".to_string())
    }

    fn parse_object(&mut self) -> Result<HashMap<String, JsonValue>, String> {
        self.chars.next(); // consume '{'
        let mut map = HashMap::new();
        self.skip_whitespace();
        if let Some(&'}') = self.chars.peek() {
            self.chars.next();
            return Ok(map);
        }
        loop {
            self.skip_whitespace();
            let key = match self.chars.peek() {
                Some('"') => self.parse_string()?,
                _ => return Err("expected string key in object".to_string()),
            };
            self.skip_whitespace();
            match self.chars.next() {
                Some(':') => {}
                _ => return Err("expected ':' after object key".to_string()),
            }
            let val = self.parse_value()?;
            map.insert(key, val);
            self.skip_whitespace();
            match self.chars.next() {
                Some(',') => {
                    self.skip_whitespace();
                    if let Some(&'}') = self.chars.peek() {
                        self.chars.next();
                        return Ok(map);
                    }
                }
                Some('}') => return Ok(map),
                _ => return Err("expected ',' or '}' in object".to_string()),
            }
        }
    }

    fn parse_array(&mut self) -> Result<Vec<JsonValue>, String> {
        self.chars.next(); // consume '['
        let mut arr = Vec::new();
        self.skip_whitespace();
        if let Some(&']') = self.chars.peek() {
            self.chars.next();
            return Ok(arr);
        }
        loop {
            let val = self.parse_value()?;
            arr.push(val);
            self.skip_whitespace();
            match self.chars.next() {
                Some(',') => {
                    self.skip_whitespace();
                    if let Some(&']') = self.chars.peek() {
                        self.chars.next();
                        return Ok(arr);
                    }
                }
                Some(']') => return Ok(arr),
                _ => return Err("expected ',' or ']' in array".to_string()),
            }
        }
    }

    fn parse_bool(&mut self) -> Result<bool, String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_alphabetic() {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        match s.as_str() {
            "true" => Ok(true),
            "false" => Ok(false),
            _ => Err(format!("invalid boolean: {s}")),
        }
    }

    fn parse_null(&mut self) -> Result<(), String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_alphabetic() {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        if s == "null" {
            Ok(())
        } else {
            Err(format!("invalid null: {s}"))
        }
    }

    fn parse_number(&mut self) -> Result<f64, String> {
        let mut s = String::new();
        while let Some(&c) = self.chars.peek() {
            if c.is_ascii_digit() || c == '.' || c == '-' || c == '+' || c == 'e' || c == 'E' {
                s.push(self.chars.next().unwrap());
            } else {
                break;
            }
        }
        s.parse::<f64>()
            .map_err(|e| format!("invalid number '{s}': {e}"))
    }
}

pub fn escape_json(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 8);
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            '\u{08}' => out.push_str("\\b"),
            '\u{0C}' => out.push_str("\\f"),
            c if (c as u32) < 0x20 => {
                use std::fmt::Write as _;
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => out.push(c),
        }
    }
    out
}

pub fn url_decode(s: &str) -> String {
    let mut bytes = Vec::with_capacity(s.len());
    let mut iter = s.as_bytes().iter().copied();
    while let Some(b) = iter.next() {
        if b == b'%' {
            let h1 = iter.next();
            let h2 = iter.next();
            if let (Some(c1), Some(c2)) = (h1, h2) {
                let hex = [c1, c2];
                if let Ok(hex_str) = std::str::from_utf8(&hex) {
                    if let Ok(val) = u8::from_str_radix(hex_str, 16) {
                        bytes.push(val);
                        continue;
                    }
                }
                bytes.push(b'%');
                bytes.push(c1);
                bytes.push(c2);
            } else {
                bytes.push(b'%');
                if let Some(c1) = h1 {
                    bytes.push(c1);
                }
            }
        } else if b == b'+' {
            bytes.push(b' ');
        } else {
            bytes.push(b);
        }
    }
    String::from_utf8_lossy(&bytes).to_string()
}

pub fn parse_target(target: &str) -> (&str, HashMap<String, String>) {
    let (path, query_str) = match target.split_once('?') {
        Some((p, q)) => (p, Some(q)),
        None => (target, None),
    };

    let mut params = HashMap::new();
    if let Some(q) = query_str {
        for pair in q.split('&') {
            if pair.is_empty() {
                continue;
            }
            let (k, v) = match pair.split_once('=') {
                Some((k, v)) => (k, v),
                None => (pair, ""),
            };
            params.insert(url_decode(k), url_decode(v));
        }
    }
    (path, params)
}
