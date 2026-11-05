# printer-web with the ricoh-spc240 driver, for the Ricoh Aficio SP C240DN
# (GDI-only). The driver is a static Go binary; it calls gs for PDF input.
FROM ghcr.io/kipavy/ricoh-spc240 AS driver

FROM ghcr.io/kipavy/printer-web
COPY --from=driver /usr/local/bin/ricoh-spc240 /usr/local/bin/ricoh-spc240
COPY --from=driver /usr/share/ricoh-spc240 /usr/share/ricoh-spc240
