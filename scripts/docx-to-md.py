#!/usr/bin/env python3
"""Convert a .docx to markdown, preserving headings, tables and links.

Written for docs/agent-architecture-baseline.md, whose editable original
is a .docx kept outside the repository. The point is that regenerating
is reproducible: a revised original should produce a diff you can read,
not a wholesale rewrite because the conversion drifted.

It handles the three things a hand-rolled text extraction loses and that
the baseline document depends on:

  headings, so the section numbers issues cite (§1-§18) survive;
  tables, which carry the configuration and contract fields rather than
  merely illustrating them;
  hyperlinks, whose targets live in the relationships part and are
  simply absent from the paragraph text.

Everything else is deliberately not handled. Bold, italics, colours and
list nesting carry no meaning in this document, and a converter that
guesses at them produces churn on every regeneration.

    scripts/docx-to-md.py INPUT.docx > OUTPUT.md

The output has no title block or front matter: the destination file
supplies its own header. See docs/agent-architecture-baseline.md.
"""

import sys
import zipfile
from xml.etree import ElementTree as ET

W = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"
R = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
NS = {"w": W, "r": R}


def convert(path):
    z = zipfile.ZipFile(path)
    rels = {
        rel.get("Id"): rel.get("Target")
        for rel in ET.fromstring(z.read("word/_rels/document.xml.rels"))
    }
    body = ET.fromstring(z.read("word/document.xml")).find("w:body", NS)

    def text_of(el):
        """Paragraph text, with hyperlinks rendered as markdown links."""
        parts = []
        for child in el:
            plain = "".join(t.text or "" for t in child.iter(f"{{{W}}}t"))
            if child.tag.split("}")[1] == "hyperlink":
                target = rels.get(child.get(f"{{{R}}}id"), "")
                parts.append(f"[{plain}]({target})" if target and plain else plain)
            else:
                parts.append(plain)
        return "".join(parts)

    def style_of(el):
        s = el.find("./w:pPr/w:pStyle", NS)
        return s.get(f"{{{W}}}val") if s is not None else ""

    def cell_of(tc):
        # A table cell holds paragraphs; markdown cells hold one line.
        lines = [text_of(p).strip() for p in tc.findall("w:p", NS)]
        return " ".join(x for x in lines if x)

    out = []
    for el in body:
        tag = el.tag.split("}")[1]
        if tag == "p":
            text, style = text_of(el).strip(), style_of(el)
            if not text:
                out.append("")
            elif style.startswith("Heading"):
                level = int("".join(c for c in style if c.isdigit()) or 1)
                out += ["", "#" * level + " " + text, ""]
            elif style.startswith("ListParagraph"):
                out.append("- " + text)
            else:
                out += [text, ""]
        elif tag == "tbl":
            rows = [
                [cell_of(tc) for tc in tr.findall("w:tc", NS)]
                for tr in el.findall("w:tr", NS)
            ]
            rows = [r for r in rows if any(r)]
            if not rows:
                continue
            width = max(len(r) for r in rows)
            rows = [r + [""] * (width - len(r)) for r in rows]
            esc = lambda row: "| " + " | ".join(c.replace("|", "\\|") for c in row) + " |"
            out += [""] + [esc(rows[0]), "|" + "---|" * width] + [esc(r) for r in rows[1:]] + [""]

    # Collapse blank runs; Word emits many and markdown needs one.
    collapsed = []
    for line in out:
        if line == "" and collapsed and collapsed[-1] == "":
            continue
        collapsed.append(line)
    return "\n".join(collapsed).strip()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    print(convert(sys.argv[1]))
