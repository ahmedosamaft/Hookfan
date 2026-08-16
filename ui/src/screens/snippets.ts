/**
 * Copy-pasteable receiver implementations.
 *
 * The spec calls this the single highest-value UI detail: it removes the
 * guesswork from onboarding a service. Each snippet does exactly what a
 * receiver must do — compare the token, verify the HMAC over the raw body in
 * constant time, and echo the challenge.
 */
export const SNIPPET_LANGUAGES = ['Go', 'Node', 'Python', 'Java'] as const
export type SnippetLanguage = (typeof SNIPPET_LANGUAGES)[number]

export function snippet(lang: SnippetLanguage, url: string): string {
  const path = safePath(url)
  return SNIPPETS[lang](path)
}

function safePath(url: string): string {
  try {
    return new URL(url).pathname || '/hook'
  } catch {
    return '/hook'
  }
}

const SNIPPETS: Record<SnippetLanguage, (path: string) => string> = {
  Go: path => `package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// Set this to the link token hookfan generated for your service.
var linkToken = []byte(os.Getenv("HOOKFAN_TOKEN"))

func handler(w http.ResponseWriter, r *http.Request) {
	// Read the raw body BEFORE decoding: the signature covers these exact bytes.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 1. The token identifies hookfan.
	if !hmac.Equal([]byte(r.Header.Get("X-Hookfan-Token")), linkToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. The signature proves the body was not tampered with in transit.
	mac := hmac.New(sha256.New, linkToken)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(r.Header.Get("X-Hookfan-Signature"), "sha256=")
	if !hmac.Equal([]byte(want), []byte(got)) { // constant time
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	// 3. The link handshake: echo the challenge back.
	if r.Header.Get("X-Hookfan-Event") == "link.verify" {
		var p struct {
			Challenge string \`json:"challenge"\`
		}
		json.Unmarshal(body, &p)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": p.Challenge})
		return
	}

	// 4. A real event. Return 2xx quickly; do the work asynchronously.
	//    Non-2xx is retried with backoff; 4xx (except 408/429) is never retried.
	go process(body)
	w.WriteHeader(http.StatusOK)
}

func process(body []byte) { /* your logic here */ }

func main() {
	http.HandleFunc("${path}", handler)
	http.ListenAndServe(":9000", nil)
}`,

  Node: path => `import express from 'express'
import crypto from 'node:crypto'

const app = express()
const linkToken = process.env.HOOKFAN_TOKEN

// The raw body is required: JSON.parse then re-stringify changes the bytes
// and the signature will never match.
app.use(express.raw({ type: '*/*', limit: '1mb' }))

app.post('${path}', (req, res) => {
  const body = req.body // a Buffer

  // 1. The token identifies hookfan.
  const token = req.get('X-Hookfan-Token') ?? ''
  if (!timingSafeEqualStr(token, linkToken)) {
    return res.status(401).send('unauthorized')
  }

  // 2. The signature proves the body was not tampered with in transit.
  const want = crypto.createHmac('sha256', linkToken).update(body).digest('hex')
  const got = (req.get('X-Hookfan-Signature') ?? '').replace(/^sha256=/, '')
  if (!timingSafeEqualStr(want, got)) {
    return res.status(401).send('bad signature')
  }

  // 3. The link handshake: echo the challenge back.
  if (req.get('X-Hookfan-Event') === 'link.verify') {
    const { challenge } = JSON.parse(body.toString())
    return res.json({ challenge })
  }

  // 4. A real event. Return 2xx quickly; do the work asynchronously.
  //    Non-2xx is retried with backoff; 4xx (except 408/429) is never retried.
  setImmediate(() => process(JSON.parse(body.toString())))
  res.sendStatus(200)
})

function timingSafeEqualStr(a, b) {
  const bufA = Buffer.from(a ?? '')
  const bufB = Buffer.from(b ?? '')
  // timingSafeEqual throws on a length mismatch, so check length first.
  return bufA.length === bufB.length && crypto.timingSafeEqual(bufA, bufB)
}

function process(event) { /* your logic here */ }

app.listen(9000)`,

  Python: path => `import hashlib
import hmac
import json
import os

from fastapi import BackgroundTasks, FastAPI, Header, Request, Response

app = FastAPI()

# Set this to the link token hookfan generated for your service.
LINK_TOKEN = os.environ["HOOKFAN_TOKEN"].encode()


@app.post("${path}")
async def receive(
    request: Request,
    background: BackgroundTasks,
    x_hookfan_token: str | None = Header(default=None),
    x_hookfan_signature: str | None = Header(default=None),
    x_hookfan_event: str | None = Header(default=None),
):
    # await request.body() gives the raw bytes. A Pydantic model or
    # await request.json() would re-encode them and the signature would
    # never match.
    body = await request.body()

    # 1. The token identifies hookfan.
    if not hmac.compare_digest((x_hookfan_token or "").encode(), LINK_TOKEN):
        return Response(status_code=401)

    # 2. The signature proves the body was not tampered with in transit.
    want = hmac.new(LINK_TOKEN, body, hashlib.sha256).hexdigest()
    got = (x_hookfan_signature or "").removeprefix("sha256=")
    if not hmac.compare_digest(want, got):  # constant time
        return Response(status_code=401)

    # 3. The link handshake: echo the challenge back.
    if x_hookfan_event == "link.verify":
        return {"challenge": json.loads(body)["challenge"]}

    # 4. A real event. Return 2xx quickly; do the work in the background.
    #    Non-2xx is retried with backoff; 4xx (except 408/429) is never retried.
    background.add_task(process, json.loads(body))
    return Response(status_code=200)


def process(event: dict) -> None:
    ...  # your logic here


# Run with:  uvicorn main:app --host 0.0.0.0 --port 9000`,

  Java: path => `package com.example.hookfan;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

@RestController
public class HookfanController {

    // Set this to the link token hookfan generated for your service.
    private static final byte[] LINK_TOKEN =
        System.getenv("HOOKFAN_TOKEN").getBytes(StandardCharsets.UTF_8);

    private final ObjectMapper mapper = new ObjectMapper();

    // @RequestBody byte[] gives the raw bytes. A String or a bound object
    // would re-encode them and the signature would never match.
    @PostMapping("${path}")
    public ResponseEntity<?> receive(
            @RequestBody byte[] body,
            @RequestHeader(value = "X-Hookfan-Token", required = false) String token,
            @RequestHeader(value = "X-Hookfan-Signature", required = false) String signature,
            @RequestHeader(value = "X-Hookfan-Event", required = false) String event
    ) throws Exception {

        // 1. The token identifies hookfan.
        if (token == null || !MessageDigest.isEqual(
                token.getBytes(StandardCharsets.UTF_8), LINK_TOKEN)) {
            return ResponseEntity.status(HttpStatus.UNAUTHORIZED).build();
        }

        // 2. The signature proves the body was not tampered with in transit.
        Mac mac = Mac.getInstance("HmacSHA256");
        mac.init(new SecretKeySpec(LINK_TOKEN, "HmacSHA256"));
        String want = toHex(mac.doFinal(body));
        String got = signature == null ? "" : signature.replaceFirst("^sha256=", "");
        if (!MessageDigest.isEqual(
                want.getBytes(StandardCharsets.UTF_8),
                got.getBytes(StandardCharsets.UTF_8))) {
            return ResponseEntity.status(HttpStatus.UNAUTHORIZED).build();
        }

        // 3. The link handshake: echo the challenge back.
        if ("link.verify".equals(event)) {
            JsonNode payload = mapper.readTree(body);
            return ResponseEntity.ok(
                java.util.Map.of("challenge", payload.get("challenge").asText()));
        }

        // 4. A real event. Return 2xx quickly; do the work asynchronously.
        //    Non-2xx is retried with backoff; 4xx (except 408/429) is never retried.
        process(body);
        return ResponseEntity.ok().build();
    }

    private void process(byte[] body) { /* your logic here */ }

    private static String toHex(byte[] bytes) {
        StringBuilder sb = new StringBuilder(bytes.length * 2);
        for (byte b : bytes) sb.append(String.format("%02x", b));
        return sb.toString();
    }
}`,
}
