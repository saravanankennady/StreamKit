"use client";

import { Table2, LineChart as LineChartIcon } from "lucide-react";
import * as React from "react";
import { Bar, BarChart, CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { Button } from "@/components/ui/button";
import { Table, Td, Th } from "@/components/ui/table";
import { cn } from "@/lib/utils";

// One chart component, so every chart obeys the same rules: 2px lines, bars at most 24px thick with 4px rounded
// ends, hairline grids, one y-axis, a crosshair tooltip listing every series, a legend for two series or more,
// and a table view of the same numbers. Colours are the validated palette's fixed slots, assigned in order.

export interface ChartSeries {
  key: string;
  label: string;
  /** Categorical palette slot, 1 to 8. */
  slot: number;
}

export type ChartRow = Record<string, number | string | null | undefined> & { x: number | string };

export interface ChartProps {
  title: React.ReactNode;
  description?: React.ReactNode;
  data: ChartRow[];
  series: ChartSeries[];
  kind?: "line" | "bar" | "stacked";
  formatY: (v: number) => string;
  /** Tooltip and table values. Defaults to formatY. */
  formatValue?: (v: number) => string;
  /** The y-axis only shows whole numbers. Set this for counts of things. */
  integer?: boolean;
  formatX: (x: number | string) => string;
  /** Long form of the x value, for the tooltip and the table. Defaults to formatX. */
  formatXLong?: (x: number | string) => string;
  /** Fixed time window [start, end] for rolling charts. */
  domain?: [number, number];
  /** The latest value of each series, shown beside its legend key. */
  latest?: Record<string, React.ReactNode>;
  height?: number;
  empty?: React.ReactNode;
  className?: string;
  yDomain?: [number | "auto" | ((v: number) => number), number | "auto" | ((v: number) => number)];
  testId?: string;
}

function LegendKey({ slot, kind }: { slot: number; kind: "line" | "bar" | "stacked" }) {
  const color = `var(--series-${slot})`;
  return kind === "line" ? (
    <span aria-hidden className="inline-block h-0.5 w-3 rounded-full" style={{ background: color }} />
  ) : (
    <span aria-hidden className="inline-block size-2.5 rounded-[2px]" style={{ background: color }} />
  );
}

function ChartTooltip({
  active,
  payload,
  label,
  series,
  format,
  formatX,
}: {
  active?: boolean;
  payload?: readonly { dataKey?: unknown; value?: unknown }[];
  label?: unknown;
  series: ChartSeries[];
  format: (v: number) => string;
  formatX: (x: number | string) => string;
}) {
  if (!active || !payload?.length || label === undefined) return null;
  return (
    <div className="rounded-xs border border-line-2 bg-raised px-2.5 py-1.5 text-xs shadow-md">
      <div className="mb-1 text-ink-3">{formatX(label as number | string)}</div>
      {series.map((s) => {
        const row = payload.find((p) => p.dataKey === s.key);
        const v = row?.value;
        return (
          <div key={s.key} className="flex items-center gap-1.5 py-px">
            <span aria-hidden className="inline-block h-0.5 w-2.5 rounded-full" style={{ background: `var(--series-${s.slot})` }} />
            <span className="num font-semibold text-ink">{typeof v === "number" ? format(v) : "No data"}</span>
            <span className="text-ink-3">{s.label}</span>
          </div>
        );
      })}
    </div>
  );
}

export function Chart({
  title,
  description,
  data,
  series,
  kind = "line",
  formatY,
  formatValue,
  integer,
  formatX,
  formatXLong,
  domain,
  latest,
  height = 180,
  empty,
  className,
  yDomain,
  testId,
}: ChartProps) {
  const [asTable, setAsTable] = React.useState(false);
  const fmtValue = formatValue ?? formatY;
  const fmtXLong = formatXLong ?? formatX;
  const hasData = data.some((row) => series.some((s) => typeof row[s.key] === "number"));
  // A line through one point draws nothing, so sparse history gets markers.
  const sparse = !domain && data.filter((row) => series.some((s) => typeof row[s.key] === "number")).length <= 40;
  const axisTick = { fill: "var(--chart-text)", fontSize: 11 };

  const common = {
    data,
    margin: { top: 8, right: 16, bottom: 0, left: 0 },
  };
  const grid = <CartesianGrid vertical={false} stroke="var(--chart-grid)" />;
  const xAxis = domain ? (
    <XAxis
      dataKey="x"
      type="number"
      scale="time"
      domain={domain}
      ticks={[0, 1, 2, 3].map((i) => domain[0] + ((domain[1] - domain[0]) * i) / 3)}
      interval="preserveStartEnd"
      tickFormatter={formatX}
      tick={axisTick}
      tickLine={false}
      stroke="var(--chart-axis)"
      minTickGap={32}
      allowDataOverflow
    />
  ) : (
    <XAxis dataKey="x" tickFormatter={formatX} tick={axisTick} tickLine={false} stroke="var(--chart-axis)" minTickGap={24} interval="preserveStartEnd" />
  );
  const yAxis = <YAxis tickFormatter={formatY} tick={axisTick} tickLine={false} axisLine={false} width={56} domain={yDomain} allowDecimals={!integer} />;
  const tooltip = (cursor: object) => (
    <Tooltip
      cursor={cursor}
      isAnimationActive={false}
      content={(p) => <ChartTooltip active={p.active} payload={p.payload as never} label={p.label} series={series} format={fmtValue} formatX={fmtXLong} />}
    />
  );

  return (
    <figure className={cn("m-0 flex min-w-0 flex-col bg-surface", className)} data-testid={testId}>
      <figcaption className="flex items-start justify-between gap-x-3 gap-y-1 px-3 pt-2.5">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-semibold">{title}</div>
          {description && <div className="text-xs text-ink-3">{description}</div>}
        </div>
        <Button
          variant="ghost"
          size="iconSm"
          className="-mt-1 -mr-1"
          aria-label={asTable ? `Show ${typeof title === "string" ? title : "chart"} as a chart` : `Show ${typeof title === "string" ? title : "chart"} as a table`}
          aria-pressed={asTable}
          onClick={() => setAsTable((v) => !v)}
        >
          {asTable ? <LineChartIcon /> : <Table2 />}
        </Button>
      </figcaption>
      {(series.length > 1 || latest) && (
        <div className="flex flex-wrap gap-x-4 gap-y-0.5 px-3 pt-1 text-xs">
          {series.map((s) => (
            <span key={s.key} className="inline-flex items-center gap-1.5 text-ink-2">
              <LegendKey slot={s.slot} kind={kind} />
              {s.label}
              {latest && <span className="num font-semibold text-ink">{latest[s.key] ?? "–"}</span>}
            </span>
          ))}
        </div>
      )}
      <div className="px-1 pt-1 pb-2">
        {!hasData ? (
          <div style={{ height }} className="flex items-center px-3 text-sm text-ink-3">
            {empty ?? "No data in this range."}
          </div>
        ) : asTable ? (
          <div className="scroll-quiet overflow-y-auto px-2" style={{ maxHeight: height }}>
            <Table className="text-xs">
              <caption className="sr-only">{title}</caption>
              <thead>
                <tr>
                  <Th className="px-2">Time</Th>
                  {series.map((s) => (
                    <Th key={s.key} className="px-2 text-right">
                      {s.label}
                    </Th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {[...data].reverse().map((row, i) => (
                  <tr key={i}>
                    <Td className="num px-2 py-1 whitespace-nowrap text-ink-2">{fmtXLong(row.x)}</Td>
                    {series.map((s) => {
                      const v = row[s.key];
                      return (
                        <Td key={s.key} className="num px-2 py-1 text-right whitespace-nowrap">
                          {typeof v === "number" ? fmtValue(v) : "–"}
                        </Td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </Table>
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={height} minWidth={0}>
            {kind === "line" ? (
              <LineChart {...common}>
                {grid}
                {xAxis}
                {yAxis}
                {tooltip({ stroke: "var(--chart-axis)", strokeWidth: 1 })}
                {series.map((s) => (
                  <Line
                    key={s.key}
                    type="linear"
                    dataKey={s.key}
                    name={s.label}
                    stroke={`var(--series-${s.slot})`}
                    strokeWidth={2}
                    dot={sparse ? { r: 4, fill: `var(--series-${s.slot})`, stroke: "var(--surface)", strokeWidth: 2 } : false}
                    activeDot={{ r: 4, stroke: "var(--surface)", strokeWidth: 2 }}
                    isAnimationActive={false}
                    connectNulls={false}
                  />
                ))}
              </LineChart>
            ) : (
              <BarChart {...common} barCategoryGap="15%" barGap={1}>
                {grid}
                {xAxis}
                {yAxis}
                {tooltip({ fill: "var(--sunken)" })}
                {series.map((s, i) => (
                  <Bar
                    key={s.key}
                    dataKey={s.key}
                    name={s.label}
                    fill={`var(--series-${s.slot})`}
                    stackId={kind === "stacked" ? "a" : undefined}
                    maxBarSize={24}
                    radius={kind === "stacked" && i < series.length - 1 ? 0 : [4, 4, 0, 0]}
                    stroke="var(--surface)"
                    strokeWidth={kind === "stacked" ? 2 : 0}
                    isAnimationActive={false}
                  />
                ))}
              </BarChart>
            )}
          </ResponsiveContainer>
        )}
      </div>
    </figure>
  );
}
