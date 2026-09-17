# The lib/** audit: a byte question asked of a character API

W5 gave the string a UTF-16 read API (length, charAt, substring, indexOf,
compareTo, hashCode and StringBuilder's indices are code units) and a WTF-8
storage with one byte boundary (getBytes, println, System.out). Code written
when `length()` was a byte count still compiles, so this is the inventory of
every place in `lib/*.teyru` that could be asking a byte question of that API.
It is the audit the plan asks for as its item 8: the inventory is the
deliverable, the fixes follow from it.

Audited revision: `adf58e7` (the read side and the write side both landed);
the four UNSURE rows were worked on `d7fde38`.
The line numbers below are that revision's, after the fixes this branch
carries, so every row names a line that exists.

## How the sites were found

A scanner walks `lib/*.teyru` with comments and literals masked and records
every use of a String read API (`length`, `charAt`, `substring`, `indexOf`,
`lastIndexOf`, `getBytes`, `toCharArray`, `codePoint*`, `getChars`, `split`,
`startsWith`, `endsWith`, `contains`, `compareTo*`, `replace*`, `trim`,
`strip*`, `toUpper/LowerCase`) together with the declaration it sits in, and
marks it **byte context** when that declaration's body mentions a byte at all
(`byte[]`, `getBytes`, `stringFrom`, `toByteArray`, `& 0xff`, a hex literal, a
charset, a length in a wire format). The audit then reads every byte-context
site and, separately, the three tells that do not need a byte word in scope:
a hand-rolled UTF-8 decoder, a step by a multi-byte character's length after
matching it, and a comment that says bytes. Nothing outside those sets is.

That rule is what makes the remaining rows CHARS: a site in a declaration that
cannot see a byte cannot be asking about one. The rule is stated because it is
how the numbers below were produced, and every exception to it is a row here.

## Totals

| | sites |
|---|---|
| String read-API sites in `lib/*.teyru` | 1688, in 37 files |
| of those, in a declaration that mentions bytes | 182, in 16 files |
| **BYTES** | 37 rows (15 code, 22 comment) |
| **CHARS** | 145 rows (the byte-context sites not listed below, plus every other site) |
| **UNSURE** | 4 rows, all four worked in the section below (three fixed, one decided) |

All 15 BYTES rows are fixed on this branch. Each code fix has a test that was
red before it and is listed in the row; the tests are t253..t263 in
`teyru-lang/tests`, and their expectations come from OpenJDK 21.0.11+10
except where the behaviour is this library's own (t255's socket half, t256,
t263, which pins a decision the JDK has no counterpart for).

## BYTES

| file:line | code | verdict | the byte question |
|---|---|---|---|
| `lib/02_string.teyru:255` | `/* StringBuilder is the mutable buffer behind string building and String.format,` | BYTES (comment) | "StringBuilder is the mutable byte buffer" |
| `lib/10_json.teyru:866` | `// Java's getAsString().charAt(0), which is what Gson answers: a char is one` | BYTES (comment) | "the first byte of a UTF-8 string" |
| `lib/10_json.teyru:1488` | `/* Gson's replacement table, applied one code unit at a time. What is not` | BYTES (comment) | "Gson's replacement table, applied byte by byte" |
| `lib/10_json.teyru:2288` | `/* Everything between the quotes is copied with substring and never re-appended` | BYTES (comment) | "the bytes of a multi-byte character are copied" |
| `lib/15_net.teyru:268` | `// The way back from bytes to text. String.getBytes is the way out and is` | BYTES (comment) | "Nothing is decoded: one byte is one character" |
| `lib/21_regex.teyru:19` | `//   - Matching is over UTF-16 code units, which is the unit this library's` | BYTES (comment) | "Matching is over bytes, because ... counts bytes" |
| `lib/24_text.teyru:389` | `Two of the symbols Java keeps as chars are Strings here: the per-mill sign` | BYTES (comment) | "multi-byte in UTF-8 and a Teyru char is a byte" |
| `lib/24_text.teyru:2021` | `// the run is appended as the text it is and not one code unit at a` | BYTES (comment) | "appended as text and not byte by byte" |
| `lib/24_text.teyru:3288` | `boolean strict = segment.charAt(sep) == '<'` | BYTES (code, T259) | `sep + 3` stepped over U+2264 by its UTF-8 length |
| `lib/24_text.teyru:3289` | `int after = sep + 1` | BYTES (code, T259) | the "<=" spelling was read as a strict "<" |
| `lib/25_util_misc.teyru:1144` | `/* The code unit at i - 1, taken as the substring it is; appending the` | BYTES (comment) | "One byte of the source, not the char" |
| `lib/30_websocket.teyru:531` | `extra = textLimit(text, WebSocketLimits.MAX_CONTROL_BYTES - 2)` | BYTES (code, T256) | the cut at 123 bytes could leave half a character |
| `lib/32_http_client.teyru:165` | `b.append("Content-Length: ").append(body.getBytes().length).append("\r\n")` | BYTES (code, T255) | Content-Length from `body.length()` |
| `lib/32_http_client.teyru:234` | `return "HTTP " + status + " " + bodyBytes.length + " bytes"` | BYTES (code, T257) | `toString` said bytes and printed characters |
| `lib/32_http_client.teyru:355` | `byte[] all = raw.getBytes()` | BYTES (code, T257) | the body was taken as a text range, not a byte range |
| `lib/32_http_client.teyru:391` | `res.bodyBytes = new byte[length]` | BYTES (code, T257) | `length <= body.length()` compared bytes with characters |
| `lib/32_http_client.teyru:410` | `static byte[] unchunkBytes(byte[] raw) {` | BYTES (code, T257) | unchunk took a chunk's byte size as a character count |
| `lib/33_spring.teyru:320` | `req.addHeader("Content-Length", "" + body.getBytes().length)` | BYTES (code, T255) | Content-Length from `body.length()` |
| `lib/42_iostream.teyru:22` | `* A string is text and a stream is bytes, and where a byte is wanted this file` | BYTES (comment) | "A Teyru string is UTF-8 bytes" |
| `lib/42_iostream.teyru:36` | `* deprecated DataInputStream.readLine, and the Writer methods that take a` | BYTES (comment) | "a char[] here is bytes" |
| `lib/42_iostream.teyru:184` | `* It extends the prelude's InputStream so it can be handed to anything that` | BYTES (comment) | "one byte is one character here" |
| `lib/42_iostream.teyru:313` | `* It implements both of this file's output shapes: an OutputStream for the data` | BYTES (comment) | "because one byte is one character" |
| `lib/42_iostream.teyru:382` | `/* The Writer half: the string's UTF-8, which is what a Writer writes and the` | BYTES (comment) | "the string's own bytes, which is the identity" |
| `lib/42_iostream.teyru:410` | `/* Java's toString() decodes the bytes with the default charset, which is UTF-8` | BYTES (comment) | "the bytes themselves" |
| `lib/42_iostream.teyru:438` | `* Both directions are written over code units and not over the string's bytes,` | BYTES (comment, fixed) | was "the last sentence here said a lone surrogate comes back as `?`"; it is Java's three bytes now (t264) |
| `lib/42_iostream.teyru:495` | `/* Java's message, which names the string and not only the length: its` | BYTES (comment) | "The ends are eight BYTES here" |
| `lib/42_iostream.teyru:586` | `* writeBytes and writeChars, Java's two String writers that are defined on` | BYTES (comment) | "writeChars has no UTF-16 to write" |
| `lib/42_iostream.teyru:703` | `* a byte is a character when a byte stream is read as one, so a data stream can` | BYTES (comment) | "a byte is a character here" |
| `lib/42_iostream.teyru:133` | `return v` | BYTES (code, T254) | `return v & 255` in BufferedReader.read() over a String |
| `lib/42_iostream.teyru:950` | `* autoFlush. Both halves are here: the Writer half writes the text it is given,` | BYTES (comment) | "a Teyru string's bytes are already UTF-8" |
| `lib/43_util_extra.teyru:86` | `/* \p{javaWhitespace}, which is Character.isWhitespace and not a rule of this` | BYTES (comment, fixed) | was "the ten characters of it that are ASCII" and "a Teyru char is one byte"; the set is Java's own now (t261) |
| `lib/43_util_extra.teyru:106` | `while (pos < input.length() && isWhitespace(input.charAt(pos))) {` | BYTES (code, T253) | `input.charAt(pos) & 255` in skipDelimiters |
| `lib/43_util_extra.teyru:120` | `while (pos < input.length() && !isWhitespace(input.charAt(pos))) {` | BYTES (code, T253) | `input.charAt(pos) & 255` in take() |
| `lib/43_util_extra.teyru:222` | `*     are Java's Scanner's, which are the whole of` | BYTES (comment, fixed) | was "the three terminators java.io uses and on nothing else" / "in a UTF-8 string they are two-byte sequences"; the set is Java's own now (t262) |
| `lib/43_util_extra.teyru:236` | `int c = input.charAt(pos)` | BYTES (code, T253) | `input.charAt(pos) & 255` in nextLine() |
| `lib/43_util_extra.teyru:245` | `if (pos < input.length() && input.charAt(pos) == 10) {` | BYTES (code, T253) | `(input.charAt(pos) & 255) == 10` in nextLine() |
| `lib/47_wiretest.teyru:211` | `res.body = Net.stringFrom(res.bodyBytes, 0, res.bodyBytes.length)` | BYTES (code, T258) | an unframed body was built through a String |

## The four UNSURE rows, worked

The audit left four rows open rather than guessing. Three of them turned out to
be fixable with the JDK as the arbiter and one is a decision; all four are
settled here.

| file:line | was | became |
|---|---|---|
| `lib/42_iostream.teyru` (was the lone-surrogate row) | `writeUTF` of a lone surrogate wrote `?` (Java `0003EDA0BD`, here `00013F`) | **fixed** in `lib/42_iostream.teyru`'s `ModifiedUtf8`: both directions walk code units now, as Java's do, and a lone surrogate is written as itself and read back as itself. `t264_modified_utf8` prints the bytes and the round trip for eight strings; its output is `t264_modified_utf8.java.ref`'s on OpenJDK 21.0.11+10. This also removed the hand-rolled UTF-8 decoder that `encode` began with -- it is tell #1 of this audit, and it was reading a string's bytes to find characters in them. |
| `lib/43_util_extra.teyru:96` | the Scanner delimiter set was `\p{javaWhitespace}` narrowed to its ASCII ten | **fixed**: the set is `Character.isWhitespace`, which is what `\p{javaWhitespace}` is, so U+3000, U+2000..U+200A, U+205F and U+1680 separate tokens and U+00A0, U+2007 and U+202F do not. `t261_scanner_delimiters` asks sixteen code points, against OpenJDK 21.0.11+10. |
| `lib/43_util_extra.teyru:237` | the line terminators were LF, CRLF and CR only | **fixed**: the terminators are Java's own set, `\r\n|[\n\r\u2028\u2029\u0085]`. `t262_scanner_line_terminators` reads thirteen strings, against the same JDK. |
| `lib/42_iostream.teyru:186` | `ByteArrayInputStream` and `DataInputStream` as Readers answer one byte per character where Java's `InputStreamReader` decodes | **decided, not changed.** `read()` is one method with two contracts -- `InputStream.read()` is "the next byte as 0..255" and `Reader.read()` over it would be "the next character" -- and the byte contract is the one the class can keep: it IS a byte array, and `read(byte[])`, `skip()` and `available()` are over the same position. Java never chooses: its byte array stream is not a Reader, and decoding is a second object (`InputStreamReader`, or this library's `Net.stringFrom`). The tree already documents it at `lib/42_iostream.teyru`'s `Reader` interface ("here one byte is one character, so a buffer read is a loop of read() calls") and in the two class comments; `t263_reader_over_bytes` pins the behaviour so that reversing the decision is a diff and not a silence. |

## The other 182 byte-context sites: CHARS

Every one of these was read. They are here rather than left implied because
each is a place where bytes are in play and the answer was still "the code
means characters": a `getBytes()` that asks the boundary for bytes, a walk
over a `byte[]`, a count of characters for a parser (`HexFormat.parseHex`
reads hex text, `UUID.fromString` reads hex text), or formatting that
happens to be `0x`-shaped.

| file:line | code | verdict |
|---|---|---|
| `lib/10_json.teyru:868` | `return (char) s.charAt(0)` | CHARS |
| `lib/10_json.teyru:1493` | `int length = value.length()` | CHARS |
| `lib/10_json.teyru:1494` | `out.append('"')` | CHARS |
| `lib/10_json.teyru:1497` | `int c = (int) value.charAt(i)` | CHARS |
| `lib/10_json.teyru:1504` | `out.append(value.substring(last, i))` | CHARS |
| `lib/10_json.teyru:1505` | `out.append(c == 0x2028 ? "\\u2028" : "\\u2029")` | CHARS |
| `lib/10_json.teyru:1494` | `out.append('"')` | CHARS |
| `lib/10_json.teyru:1549` | `sb.append("\\u00")` | CHARS |
| `lib/10_json.teyru:1550` | `sb.append(hexDigit((c >> 4) & 15))` | CHARS |
| `lib/10_json.teyru:1551` | `sb.append(hexDigit(c & 15))` | CHARS |
| `lib/10_json.teyru:2119` | `return src.substring(start, pos)` | CHARS |
| `lib/10_json.teyru:2294` | `char quote = src.charAt(pos)` | CHARS |
| `lib/10_json.teyru:2310` | `sb.append(src.substring(start, pos))` | CHARS |
| `lib/10_json.teyru:2310` | `sb.append(src.substring(start, pos))` | CHARS |
| `lib/10_json.teyru:2398` | `if (pos + 1 < src.length() && src.charAt(pos) == '\\' && src.charAt(pos + 1) == 'u') {` | CHARS |
| `lib/10_json.teyru:2403` | `sb.append((char) cp)` | CHARS |
| `lib/10_json.teyru:2404` | `sb.append((char) low)` | CHARS |
| `lib/10_json.teyru:2409` | `sb.append(REPLACEMENT)` | CHARS |
| `lib/10_json.teyru:2409` | `sb.append(REPLACEMENT)` | CHARS |
| `lib/10_json.teyru:2403` | `sb.append((char) cp)` | CHARS |
| `lib/14_util_arrays.teyru:337` | `return x.compareTo(b)` | CHARS |
| `lib/14_util_arrays.teyru:1192` | `sb.append("]")` | CHARS |
| `lib/15_net.teyru:412` | `byte[] all = s.getBytes()` | CHARS |
| `lib/15_net.teyru:914` | `sb.append(Net.stringFrom(buf, pos, count - pos))` | CHARS |
| `lib/15_net.teyru:930` | `sb.append(Net.stringFrom(buf, pos, nl - pos))` | CHARS |
| `lib/15_net.teyru:934` | `if (line.endsWith("\r")) {` | CHARS |
| `lib/15_net.teyru:935` | `line = line.substring(0, line.length() - 1)` | CHARS |
| `lib/18_web.teyru:137` | `return part(body.getBytes(), boundary, name)` | CHARS |
| `lib/18_web.teyru:156` | `return parts(body.getBytes(), boundary)` | CHARS |
| `lib/18_web.teyru:172` | `byte[] delimiter = ("--" + boundary).getBytes()` | CHARS |
| `lib/18_web.teyru:173` | `byte[] closing = "--".getBytes()` | CHARS |
| `lib/18_web.teyru:174` | `byte[] blankLine = "\r\n\r\n".getBytes()` | CHARS |
| `lib/18_web.teyru:263` | `return f.name.length() == 0 ? null : f` | CHARS |
| `lib/18_web.teyru:353` | `String charset = MultipartForm.parameter(header("Content-Type"), "charset").toLowerCase(` | CHARS |
| `lib/18_web.teyru:357` | `b.append((char) (bodyBytes[i] & 0xff))` | CHARS |
| `lib/18_web.teyru:382` | `bodyBytes = text.getBytes()` | CHARS |
| `lib/18_web.teyru:761` | `b.append("Connection: ").append(keepAlive ? "keep-alive" : "close").append("\r\n")` | CHARS |
| `lib/18_web.teyru:759` | `b.append("\r\n")` | CHARS |
| `lib/18_web.teyru:777` | `return body.getBytes().length` | CHARS |
| `lib/18_web.teyru:2342` | `head.append(line).append("\r\n")` | CHARS |
| `lib/18_web.teyru:2351` | `return line.getBytes().length` | CHARS |
| `lib/21_regex.teyru:333` | `char c = src.charAt(pos)` | CHARS |
| `lib/21_regex.teyru:375` | `if (pos < src.length() && src.charAt(pos) == ',') {` | CHARS |
| `lib/21_regex.teyru:382` | `if (pos >= src.length() \|\| src.charAt(pos) != '}') {` | CHARS |
| `lib/23_math.teyru:2439` | `buf.append(this.intVal.toString())` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2455` | `buf.insert(insertionPoint, '.')` | CHARS |
| `lib/23_math.teyru:2457` | `buf.insert(0, '-')` | CHARS |
| `lib/23_math.teyru:2462` | `buf.append(signum < 0 ? "-0." : "0.")` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2466` | `buf.append(intString)` | CHARS |
| `lib/23_math.teyru:2482` | `buf.append('-')` | CHARS |
| `lib/23_math.teyru:2484` | `int coeffLen = coeff.length()` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2490` | `buf.append('.')` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2494` | `buf.append(coeff)` | CHARS |
| `lib/23_math.teyru:2496` | `buf.append(coeff.substring(0, -pad))` | CHARS |
| `lib/23_math.teyru:2490` | `buf.append('.')` | CHARS |
| `lib/23_math.teyru:2498` | `buf.append(coeff.substring(-pad))` | CHARS |
| `lib/23_math.teyru:2502` | `buf.append(coeff.charAt(0))` | CHARS |
| `lib/23_math.teyru:2490` | `buf.append('.')` | CHARS |
| `lib/23_math.teyru:2505` | `buf.append(coeff.substring(1))` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2518` | `buf.append("0.00")` | CHARS |
| `lib/23_math.teyru:2521` | `buf.append("0.0")` | CHARS |
| `lib/23_math.teyru:2494` | `buf.append(coeff)` | CHARS |
| `lib/23_math.teyru:2441` | `buf.append('0')` | CHARS |
| `lib/23_math.teyru:2530` | `buf.append(coeff.substring(0, sig))` | CHARS |
| `lib/23_math.teyru:2490` | `buf.append('.')` | CHARS |
| `lib/23_math.teyru:2532` | `buf.append(coeff.substring(sig))` | CHARS |
| `lib/23_math.teyru:2536` | `buf.append('E')` | CHARS |
| `lib/23_math.teyru:2538` | `buf.append('+')` | CHARS |
| `lib/23_math.teyru:2540` | `buf.append(adjusted)` | CHARS |
| `lib/24_text.teyru:378` | `return lhs.compareTo(rhs)` | CHARS |
| `lib/25_util_misc.teyru:373` | `long mostSigBits = Long.parseLong(name.substring(0, dash1), 16) & 0xffffffffL` | CHARS |
| `lib/25_util_misc.teyru:375` | `mostSigBits = mostSigBits \| (Long.parseLong(name.substring(dash1 + 1, dash2), 16) & 0xf` | CHARS |
| `lib/25_util_misc.teyru:375` | `mostSigBits = mostSigBits \| (Long.parseLong(name.substring(dash2 + 1, dash3), 16) & 0xf` | CHARS |
| `lib/25_util_misc.teyru:378` | `long leastSigBits = Long.parseLong(name.substring(dash3 + 1, dash4), 16) & 0xffffL` | CHARS |
| `lib/25_util_misc.teyru:380` | `leastSigBits = leastSigBits \| (Long.parseLong(name.substring(dash4 + 1), 16) & 0xffffff` | CHARS |
| `lib/25_util_misc.teyru:452` | `return Long.toHexString(high \| (value & (high - 1))).substring(1)` | CHARS |
| `lib/25_util_misc.teyru:968` | `sb.append("}")` | CHARS |
| `lib/25_util_misc.teyru:1333` | `for (int i = 0 : i < s.length() : i++) {` | CHARS |
| `lib/25_util_misc.teyru:1337` | `out.append('\\')` | CHARS |
| `lib/25_util_misc.teyru:1339` | `out.append(c)` | CHARS |
| `lib/25_util_misc.teyru:1359` | `out.append("\\u" + hex4(c))` | CHARS |
| `lib/25_util_misc.teyru:1359` | `out.append("\\u" + hex4(c))` | CHARS |
| `lib/25_util_misc.teyru:1380` | `sb.append(digits.charAt((c >> shift) & 0xF))` | CHARS |
| `lib/25_util_misc.teyru:1787` | `b.append(ALPHABET.charAt((n >> 18) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1788` | `b.append(ALPHABET.charAt((n >> 12) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1789` | `b.append(ALPHABET.charAt((n >> 6) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1790` | `b.append(ALPHABET.charAt(n & 63))` | CHARS |
| `lib/25_util_misc.teyru:1787` | `b.append(ALPHABET.charAt((n >> 18) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1788` | `b.append(ALPHABET.charAt((n >> 12) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1797` | `b.append("==")` | CHARS |
| `lib/25_util_misc.teyru:1787` | `b.append(ALPHABET.charAt((n >> 18) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1788` | `b.append(ALPHABET.charAt((n >> 12) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1789` | `b.append(ALPHABET.charAt((n >> 6) & 63))` | CHARS |
| `lib/25_util_misc.teyru:1805` | `b.append("=")` | CHARS |
| `lib/25_util_misc.teyru:1814` | `return encodeToString(text.getBytes())` | CHARS |
| `lib/30_websocket.teyru:111` | `out.write(WebSocketFrame.encode(WebSocketFrame.TEXT, text.getBytes()))` | CHARS |
| `lib/30_websocket.teyru:388` | `return Base64.encodeToString(Sha1.of((key + GUID).getBytes()))` | CHARS |
| `lib/30_websocket.teyru:528` | `byte[] text = reason == null ? new byte[0] : reason.getBytes()` | CHARS |
| `lib/32_http_client.teyru:164` | `if (body.length() > 0 && header("Content-Length").length() == 0) {` | CHARS |
| `lib/32_http_client.teyru:167` | `if (header("Connection").length() == 0) {` | CHARS |
| `lib/32_http_client.teyru:168` | `b.append("Connection: keep-alive\r\n")` | CHARS |
| `lib/32_http_client.teyru:170` | `b.append("\r\n")` | CHARS |
| `lib/32_http_client.teyru:171` | `b.append(body)` | CHARS |
| `lib/32_http_client.teyru:349` | `int headEnd = raw.indexOf("\r\n\r\n")` | CHARS |
| `lib/32_http_client.teyru:350` | `String head = headEnd < 0 ? raw : raw.substring(0, headEnd)` | CHARS |
| `lib/32_http_client.teyru:356` | `int bodyStart = headEnd < 0 ? all.length : raw.substring(0, headEnd + 4).getBytes().leng` | CHARS |
| `lib/32_http_client.teyru:407` | `return new String(unchunkBytes(raw.getBytes()))` | CHARS |
| `lib/33_spring.teyru:314` | `if (body.length() > 0) {` | CHARS |
| `lib/33_spring.teyru:316` | `req.addHeader("Content-Type", contentType.length() > 0 ? contentType : "application/json` | CHARS |
| `lib/41_hexformat.teyru:129` | `out.append(prefix)` | CHARS |
| `lib/41_hexformat.teyru:130` | `out.append(digit(digits, (bytes[i] >> 4) & 0xF))` | CHARS |
| `lib/41_hexformat.teyru:131` | `out.append(digit(digits, bytes[i] & 0xF))` | CHARS |
| `lib/41_hexformat.teyru:132` | `out.append(suffix)` | CHARS |
| `lib/41_hexformat.teyru:151` | `if (string.length() == 0) {` | CHARS |
| `lib/41_hexformat.teyru:154` | `if (delimiter.length() == 0 && prefix.length() == 0 && suffix.length() == 0) {` | CHARS |
| `lib/41_hexformat.teyru:164` | `checkLiteral(string, string.length() - suffix.length(), suffix)` | CHARS |
| `lib/41_hexformat.teyru:166` | `int len = (string.length() - valueChars) / stride + 1` | CHARS |
| `lib/41_hexformat.teyru:168` | `int offset = prefix.length()` | CHARS |
| `lib/41_hexformat.teyru:173` | `offset = offset + 2 + between.length()` | CHARS |
| `lib/41_hexformat.teyru:198` | `byte[] bytes = new byte[string.length() / 2]` | CHARS |
| `lib/41_hexformat.teyru:206` | `return (HexFormat.fromHexDigit(string.charAt(index)) << 4) \|` | CHARS |
| `lib/41_hexformat.teyru:207` | `HexFormat.fromHexDigit(string.charAt(index + 1))` | CHARS |
| `lib/41_hexformat.teyru:255` | `return table.charAt(value & 0xF)` | CHARS |
| `lib/42_iostream.teyru:504` | `int length = s.length()` | CHARS |
| `lib/42_iostream.teyru:505` | `return "encoded string (" + s.substring(0, 8) + "..." + s.substring(length - 8, length) ` | CHARS |
| `lib/42_iostream.teyru:1028` | `if (pending != null && pending.length() > 0) {` | CHARS |
| `lib/42_iostream.teyru:1029` | `byte[] b = pending.toString().getBytes()` | CHARS |
| `lib/42_iostream.teyru:1031` | `pending.delete(0, pending.length())` | CHARS |
| `lib/44_zip.teyru:2859` | `return compress(text.getBytes())` | CHARS |
| `lib/45_zip_archive.teyru:198` | `if (name.length() > 0xFFFF) {` | CHARS |
| `lib/45_zip_archive.teyru:510` | `byte[] bytes = c.getBytes()` | CHARS |
| `lib/45_zip_archive.teyru:778` | `byte[] nameBytes = e.name.getBytes()` | CHARS |
| `lib/45_zip_archive.teyru:778` | `byte[] nameBytes = e.name.getBytes()` | CHARS |
| `lib/45_zip_archive.teyru:818` | `byte[] commentBytes = e.comment == null ? null : e.comment.getBytes()` | CHARS |
| `lib/47_wiretest.teyru:345` | `return sendFrame(opcode, text.getBytes(), fin)` | CHARS |

## The step tell

Every `i += 2` / `i = i + 2` / `+ 3` in `lib/*.teyru` was read as well,
because a step over a non-ASCII literal by its length in bytes is a byte
question with no byte word in scope (that is how the six sites in
`lib/24_text.teyru` were found by the W5 suite failing). Twenty-eight of them
are an ASCII step -- a doubled quote, `\u` plus four hex digits, `%XX`, a
three-byte base64 group, a chunk header -- and the rest walk a `byte[]`
(`ModifiedUtf8`, the ZIP and gzip readers, `WebSocketText`) where a byte step
is the point. Two were not: the ≤ spelling of a choice limit, which
is one code unit and was stepped over as three (`lib/24_text.teyru`, in the
table above), and the close reason cut (`lib/30_websocket.teyru`, same).

## CHARS rows that are known deviations from Java

These are not byte questions and are not fixed here; they are recorded so
that the next reader does not re-derive them:

- `lib/43_util_extra.teyru` Scanner: the delimiter set and the line
  terminators are the ASCII narrowing described under UNSURE.
- `lib/42_iostream.teyru` ByteArrayInputStream/DataInputStream as Readers:
  one byte per character, where Java decodes (UNSURE).
- `lib/42_iostream.teyru` writeUTF of a lone surrogate: `?` (UNSURE).
- `lib/24_text.teyru` DecimalFormatSymbols: `getPerMill` and `getInfinity`
  answer a String where Java answers a char.
- `lib/30_websocket.teyru` closeBody: this library cuts where the JDK refuses
  an over-long close reason (`WebSocketImpl.isLegalReason` throws). The cut
  is deliberate; only where it lands was a bug.

## What is not here

- `internal/**` and `examples/**`: this audit is the plan's lib/** one.
- The runtime's own byte/text boundary (`ty_str_write`, `ty_str_of_bytes`)
  is W5's second and third pieces and is tested by t249..t252.

