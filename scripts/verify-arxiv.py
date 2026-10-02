#!/usr/bin/env python3
"""arXiv source validation only; Python standard library, no credentials/models.

Fetch once with: python3 scripts/verify-arxiv.py --download
Re-analyse cached responses with: python3 scripts/verify-arxiv.py
Raw responses and manifest are stored under gitignored data/validation/arxiv.
Each URL is requested at most once per run, in sequence, without retries.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import html
import json
from pathlib import Path
import re
import time
import urllib.error
import urllib.request
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[1]
CATEGORIES = ("cs.IR", "cs.LG", "cs.AI", "cs.CL", "stat.ML")
NS = {"atom": "http://www.w3.org/2005/Atom", "arxiv": "http://arxiv.org/schemas/atom"}
ID_RE = re.compile(r"(?:arXiv:|arXiv\.org:|/abs/)(\d{4}\.\d{4,5}|[a-zA-Z.-]+/\d{7})(v\d+)?")


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def text(element):
    return " ".join("".join(element.itertext()).split()) if element is not None else None


def parse_atom(body):
    root = ET.fromstring(body)
    if root.tag != "{http://www.w3.org/2005/Atom}feed":
        raise ValueError("Response is not an Atom feed")
    entries = []
    for entry in root.findall("atom:entry", NS):
        raw_id = text(entry.find("atom:id", NS)) or ""
        match = ID_RE.search(raw_id)
        links = [dict(link.attrib) for link in entry.findall("atom:link", NS)]
        if match is None:
            for link in links:
                match = ID_RE.search(link.get("href", ""))
                if match:
                    break
        extensions = {child.tag: text(child) for child in entry if not child.tag.startswith("{" + NS["atom"] + "}")}
        entries.append({
            "announce_type": text(entry.find("arxiv:announce_type", NS)),
            "id": match.group(1) if match else None,
            "version": int(match.group(2)[1:]) if match and match.group(2) else None,
            "raw_id": raw_id,
            "title": text(entry.find("atom:title", NS)),
            "published": text(entry.find("atom:published", NS)),
            "updated": text(entry.find("atom:updated", NS)),
            "categories": [dict(c.attrib) for c in entry.findall("atom:category", NS)],
            "extensions": extensions,
            "links": links,
        })
    return {"title": text(root.find("atom:title", NS)),
            "id": text(root.find("atom:id", NS)),
            "updated": text(root.find("atom:updated", NS)),
            "extensions": {c.tag: text(c) for c in root if not c.tag.startswith("{" + NS["atom"] + "}")},
            "count": len(entries), "entries": entries}


def parse_list(body):
    source = body.decode("utf-8", errors="replace")
    plain = html.unescape(re.sub(r"<[^>]+>", " ", source))
    plain = " ".join(plain.split())
    headings = [html.unescape(re.sub(r"<[^>]+>", " ", value)).strip()
                for value in re.findall(r"<h[1-4]\b[^>]*>(.*?)</h[1-4]>", source, flags=re.S)]
    # Each list item contains one /abs/ anchor; abstracts elsewhere are excluded
    # by requiring anchors inside the definition-list item header (<dt>).
    sections = []
    positions = list(re.finditer(r"<h[1-4]\b[^>]*>(.*?)</h[1-4]>", source, flags=re.S))
    for index, heading in enumerate(positions):
        end = positions[index + 1].start() if index + 1 < len(positions) else len(source)
        fragment = source[heading.end():end]
        ids = []
        for item in re.findall(r"<dt\b[^>]*>(.*?)</dt>", fragment, flags=re.S):
            match = re.search(r'href\s*=\s*["\'](?:https?://arxiv.org)?/abs/([^"\'#?]+)', item)
            if match:
                ids.append(match.group(1))
        if ids:
            sections.append({"heading": html.unescape(re.sub(r"<[^>]+>", " ", heading.group(1))).strip(), "count": len(ids), "ids": ids})
    ids = [item for section in sections for item in section["ids"]]
    declared = re.search(r"Total of\s+(\d+)\s+entries", plain)
    batch = re.search(r"Showing new listings for ([A-Za-z]+, \d+ [A-Za-z]+ \d{4})", plain)
    for section in sections:
        counts = re.search(r"showing (\d+) of (\d+) entries", section["heading"])
        section["declared_showing"] = int(counts.group(1)) if counts else None
        section["declared_total"] = int(counts.group(2)) if counts else None
    return {"headings": headings, "sections": sections, "count": len(ids), "ids": ids,
            "declared_total": int(declared.group(1)) if declared else None,
            "batch_date": dt.datetime.strptime(batch.group(1), "%A, %d %B %Y").date().isoformat() if batch else None,
            "count_statements": re.findall(r".{0,100}(?:Total of|total of|Showing|showing|entries|submissions).{0,160}", plain),
            "date_statements": re.findall(r".{0,80}(?:\bMon\b|\bTue\b|\bWed\b|\bThu\b|\bFri\b|\bSat\b|\bSun\b).{0,100}", plain),
            "text_excerpt": plain[:3500]}


def download(directory, history_date):
    directory.mkdir(parents=True, exist_ok=True)
    manifest_path = directory / "manifest.json"
    if manifest_path.exists():
        raise SystemExit("Cache exists: use analysis mode or choose a new --data-dir; no implicit refetch")
    manifest = {"started_at": now(), "request_timeout_seconds": 20, "minimum_interval_seconds": 3,
                "deadline_seconds": 330, "requests": []}
    start = time.monotonic()
    last_start = None
    urls = [(f"atom-{category}", f"https://rss.arxiv.org/atom/{category}", "xml") for category in CATEGORIES]
    urls += [(f"new-{category}", f"https://arxiv.org/list/{category}/new?show=1000", "html") for category in CATEGORIES]
    urls += [("recent-cs.IR", "https://arxiv.org/list/cs.IR/recent?show=250", "html"),
             ("pastweek-cs.IR", "https://arxiv.org/list/cs.IR/pastweek?show=250", "html"),
             ("dated-cs.IR", f"https://arxiv.org/list/cs.IR/{history_date}?show=1000", "html"),
             ("rss-doc", "https://info.arxiv.org/help/rss.html", "html"),
             ("api-doc", "https://info.arxiv.org/help/api/user-manual.html", "html")]
    for name, url, extension in urls:
        if time.monotonic() - start > 305:
            manifest["stopped_reason"] = "Overall deadline; remaining URLs were not requested"
            break
        if last_start is not None:
            time.sleep(max(0, 3 - (time.monotonic() - last_start)))
        last_start = time.monotonic()
        result = {"name": name, "url": url, "requested_at": now()}
        try:
            request = urllib.request.Request(url, headers={"User-Agent": "paper-digest-source-validation/1.0 (single-pass public metadata research)", "Accept": "application/atom+xml, text/html;q=0.9, */*;q=0.1"})
            with urllib.request.urlopen(request, timeout=20) as response:
                result.update({"status": response.status, "final_url": response.url, "headers": dict(response.headers)})
                body = response.read(8 * 1024 * 1024 + 1)
                result["capture_truncated"] = len(body) > 8 * 1024 * 1024
                filename = f"{name}.{extension}"
                (directory / filename).write_bytes(body)
                result.update({"file": filename, "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest()})
        except urllib.error.HTTPError as error:
            body = error.read(65536)
            filename = f"{name}.error.txt"
            (directory / filename).write_bytes(body)
            result.update({"status": error.code, "error": str(error), "file": filename,
                           "bytes": len(body), "sha256": hashlib.sha256(body).hexdigest(), "headers": dict(error.headers)})
        except (OSError, TimeoutError) as error:
            result["error"] = f"{type(error).__name__}: {error}"
        result["finished_at"] = now()
        result["duration_seconds"] = round(time.monotonic() - last_start, 3)
        manifest["requests"].append(result)
        manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")
        print(f"{name}: {result.get('status', result.get('error'))}, {result.get('bytes', 0)} bytes", flush=True)
    manifest["finished_at"] = now()
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n")


def analyze(directory):
    manifest = json.loads((directory / "manifest.json").read_text())
    results = {}
    for request in manifest["requests"]:
        name = request["name"]
        if request.get("status") != 200 or request.get("capture_truncated"):
            results[name] = {"result": "failed", "error": request.get("error", "truncated or non-200 response")}
            continue
        body = (directory / request["file"]).read_bytes()
        if hashlib.sha256(body).hexdigest() != request["sha256"]:
            raise ValueError(f"SHA256 mismatch: {name}")
        try:
            results[name] = parse_atom(body) if name.startswith("atom-") else parse_list(body)
        except (ET.ParseError, ValueError) as error:
            results[name] = {"result": "failed", "error": str(error)}
    comparisons = {}
    dedup = {}
    for category in CATEGORIES:
        atom = results.get(f"atom-{category}", {})
        listing = results.get(f"new-{category}", {})
        if "entries" not in atom or "ids" not in listing:
            comparisons[category] = {"result": "unverified", "reason": "Feed or official full list unavailable"}
            continue
        atom_ids = {entry["id"] for entry in atom["entries"]}
        list_ids = {re.sub(r"v\d+$", "", item) for item in listing["ids"]}
        publications = {entry["published"][:10] for entry in atom["entries"] if entry.get("published")}
        type_counts = {kind: sum(entry["announce_type"] == kind for entry in atom["entries"])
                       for kind in ("new", "cross", "replace", "replace-cross")}
        section_checks = []
        for section in listing["sections"]:
            heading = section["heading"]
            kinds = ("new",) if heading.startswith("New submissions") else ("cross",) if heading.startswith("Cross submissions") else ("replace", "replace-cross") if heading.startswith("Replacement submissions") else ()
            expected = {entry["id"] for entry in atom["entries"] if entry["announce_type"] in kinds}
            section_checks.append({"heading": heading,
                "complete": section["count"] == section["declared_showing"] == section["declared_total"],
                "same_ids": expected == set(section["ids"])})
        complete = (atom_ids == list_ids and None not in atom_ids and len(atom_ids) == atom["count"]
                    and all(entry["version"] is not None and entry["version"] > 0 for entry in atom["entries"])
                    and listing["count"] == listing["declared_total"]
                    and publications == {listing["batch_date"]}
                    and len(section_checks) == 3 and all(item["complete"] and item["same_ids"] for item in section_checks))
        comparisons[category] = {"result": "passed" if complete else "failed",
                                  "atom_count": atom["count"], "list_count": listing["count"],
                                  "declared_list_total": listing["declared_total"],
                                  "batch_date": listing["batch_date"], "atom_publication_dates": sorted(publications),
                                  "type_counts": type_counts, "section_checks": section_checks,
                                  "atom_only": sorted(atom_ids - list_ids, key=str),
                                  "list_only": sorted(list_ids - atom_ids, key=str),
                                  "same_ids": atom_ids == list_ids,
                                  "note": "Pass proves this captured batch only; no future completeness guarantee"}
        for entry in atom["entries"]:
            key = (entry["id"], entry["version"])
            dedup.setdefault(key, []).append(category)
    output = {"analyzed_at": now(), "sources": results, "comparisons": comparisons,
              "dedup": {"occurrences": sum(len(v) for v in dedup.values()), "unique_id_version": len(dedup),
                        "cross_category_examples": [{"id": key[0], "version": key[1], "categories": value}
                                                     for key, value in dedup.items() if len(value) > 1][:20]}}
    (directory / "analysis.json").write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"comparisons": comparisons, "dedup": output["dedup"]}, ensure_ascii=False, indent=2))


def self_check():
    fixture_dir = ROOT / "testdata/validation/arxiv"
    index = json.loads((fixture_dir / "index.json").read_text())
    for item in index["fixtures"]:
        body = (fixture_dir / item["file"]).read_bytes()
        assert hashlib.sha256(body).hexdigest() == item["sha256"], item["file"]
    real = parse_atom((fixture_dir / "real-entries.xml").read_bytes())
    assert {entry["announce_type"] for entry in real["entries"]} == {"new", "cross", "replace", "replace-cross"}
    assert all(entry["id"] and entry["version"] for entry in real["entries"])
    assert any(entry["version"] > 1 for entry in real["entries"])
    assert len({(entry["id"], entry["version"]) for entry in real["entries"]}) < real["count"]
    empty = parse_atom((fixture_dir / "synthetic-empty.xml").read_bytes())
    assert empty["count"] == 0  # Parsing an empty feed does NOT certify an empty batch.
    try:
        parse_atom((fixture_dir / "synthetic-truncated.xml").read_bytes())
    except ET.ParseError:
        pass
    else:
        raise AssertionError("Truncated XML was accepted")
    listing = parse_list((fixture_dir / "real-cs.IR-list-extract.html").read_bytes())
    assert listing["declared_total"] == listing["count"] == 41
    assert listing["batch_date"] == "2026-10-01"
    assert all(section["count"] == section["declared_total"] for section in listing["sections"])
    print("Fixture checks passed: real four announcement types/vN/dedup, synthetic empty/truncation, list counts")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-check", action="store_true", help="Offline checks against explicitly labelled fixtures")
    parser.add_argument("--download", action="store_true", help="One serial fetch, timeout 20s, no retry")
    parser.add_argument("--history-date", type=dt.date.fromisoformat, default=dt.date(2026, 10, 1),
                        help="One exploratory ISO-date list URL (default captured batch 2026-10-01); not a proven endpoint")
    parser.add_argument("--data-dir", type=Path, default=ROOT / "data/validation/arxiv")
    args = parser.parse_args()
    if args.self_check:
        self_check()
        return
    if args.download:
        download(args.data_dir, args.history_date)
    analyze(args.data_dir)

if __name__ == "__main__":
    main()
