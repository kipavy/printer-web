# Vendored front-end assets

Everything here is served by the app itself (no CDN, works offline). Files are
byte-identical to the upstream downloads; verify with `sha256sum`.

| File | Source | Licence | Bytes | sha256 |
|------|--------|---------|------:|--------|
| `pdf.min.js` | https://cdnjs.cloudflare.com/ajax/libs/pdf.js/3.11.174/pdf.min.js | Apache-2.0 (`LICENSE-pdfjs.txt`) | 320004 | 5b5799e6f8c680663207ac5b42ee14eed2a406fa7af48f50c154f0c0b1566946 |
| `pdf.worker.min.js` | https://cdnjs.cloudflare.com/ajax/libs/pdf.js/3.11.174/pdf.worker.min.js | Apache-2.0 | 1087212 | feabdf309770ed24bba31a5467836cdc8cf639c705af27d52b585b041bb8527b |
| `qrcode.min.js` | https://cdnjs.cloudflare.com/ajax/libs/qrcodejs/1.0.0/qrcode.min.js | MIT (`LICENSE-qrcodejs.txt`) | 19927 | c541ef06327885a8415bca8df6071e14189b4855336def4f36db54bde8484f36 |
| `fonts/BricolageGrotesque-latin.woff2` | https://fonts.gstatic.com/s/bricolagegrotesque/v9/3y9K6as8bTXq_nANBjzKo3IeZx8z6up5BeSl9D4dj_x9PpZBMlGIInHWVyNJ.woff2 (variable, weights 500-800, latin subset) | OFL-1.1 (`fonts/OFL-BricolageGrotesque.txt`) | 76868 | 85f55a58a31e61a2e19e8bb25fed503181bf2a6b4cab76c589992cfaac377447 |
| `fonts/AtkinsonHyperlegible-400-latin.woff2` | https://fonts.gstatic.com/s/atkinsonhyperlegible/v12/9Bt23C1KxNDXMspQ1lPyU89-1h6ONRlW45G04pIoWQeCbA.woff2 (latin subset) | OFL-1.1 (`fonts/OFL-AtkinsonHyperlegible.txt`) | 11208 | a60206dd4793eb886952e48dc3e1c1d59942c004b2ab1e0086e7ccd156a7852d |
| `fonts/AtkinsonHyperlegible-700-latin.woff2` | https://fonts.gstatic.com/s/atkinsonhyperlegible/v12/9Bt73C1KxNDXMspQ1lPyU89-1h6ONRlW45G8Wbc9dCWPRl-uFQ.woff2 (latin subset) | OFL-1.1 | 11364 | 1cc7c0c5fbf16d1be3cb6d95c89fc0afd0e11f264e239ac97c64a326c5d88fe4 |

Licence texts:

| File | Source | sha256 |
|------|--------|--------|
| `LICENSE-pdfjs.txt` | https://raw.githubusercontent.com/mozilla/pdf.js/v3.11.174/LICENSE | 0d542e0c8804e39aa7f37eb00da5a762149dc682d7829451287e11b938e94594 |
| `LICENSE-qrcodejs.txt` | https://raw.githubusercontent.com/davidshimjs/qrcodejs/master/LICENSE | aaa1f349028a59432eb00222b8ddfe4d2ccebdce04cf8348dcb8d41b7068107b |
| `fonts/OFL-BricolageGrotesque.txt` | https://raw.githubusercontent.com/google/fonts/main/ofl/bricolagegrotesque/OFL.txt | 4b5a7d8f37f5602621c8a8d7358a6a2e71317e6c231c661e15aef0275d3e07ba |
| `fonts/OFL-AtkinsonHyperlegible.txt` | https://raw.githubusercontent.com/google/fonts/main/ofl/atkinsonhyperlegible/OFL.txt | f32d22b3908fcad2c86a74000614ec22e6a7f66ea7e867e616026a27aebdc143 |
