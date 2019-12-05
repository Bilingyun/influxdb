// Libraries
import React, {FunctionComponent, useMemo} from 'react'
import {Config, Table} from '@influxdata/giraffe'

// Components
import EmptyGraphMessage from 'src/shared/components/EmptyGraphMessage'
import GraphLoadingDots from 'src/shared/components/GraphLoadingDots'

// Utils
import {useVisDomainSettings} from 'src/shared/utils/useVisDomainSettings'
import {
  getFormatter,
  geomToInterpolation,
  filterNoisyColumns,
  parseBounds,
  defaultXColumn,
  defaultYColumn,
} from 'src/shared/utils/vis'

// Constants
import {VIS_THEME} from 'src/shared/constants'
import {DEFAULT_LINE_COLORS} from 'src/shared/constants/graphColorPalettes'
import {INVALID_DATA_COPY} from 'src/shared/copy/cell'

// Types
import {RemoteDataState, XYViewProperties, TimeZone} from 'src/types'

interface Props {
  children: (config: Config) => JSX.Element
  endTime: number
  fluxGroupKeyUnion: string[]
  loading: RemoteDataState
  startTime: number
  table: Table
  timeZone: TimeZone
  viewProperties: XYViewProperties
}

const XYPlot: FunctionComponent<Props> = ({
  children,
  endTime,
  fluxGroupKeyUnion,
  loading,
  startTime,
  table,
  timeZone,
  viewProperties: {
    geom,
    colors,
    xColumn: storedXColumn,
    yColumn: storedYColumn,
    shadeBelow,
    axes: {
      x: {
        label: xAxisLabel,
        prefix: xTickPrefix,
        suffix: xTickSuffix,
        base: xTickBase,
        bounds: xBounds,
      },
      y: {
        label: yAxisLabel,
        prefix: yTickPrefix,
        suffix: yTickSuffix,
        bounds: yBounds,
        base: yTickBase,
      },
    },
    position,
    timeFormat,
  },
}) => {
  const storedXDomain = useMemo(() => parseBounds(xBounds), [xBounds])
  const storedYDomain = useMemo(() => parseBounds(yBounds), [yBounds])

  const xColumn = storedXColumn || defaultXColumn(table)
  const yColumn = storedYColumn || defaultYColumn(table)

  const columnKeys = table.columnKeys

  const [xDomain, onSetXDomain, onResetXDomain] = useVisDomainSettings(
    storedXDomain,
    table.getColumn(xColumn, 'number'),
    startTime,
    endTime
  )

  const [yDomain, onSetYDomain, onResetYDomain] = useVisDomainSettings(
    storedYDomain,
    table.getColumn(yColumn, 'number')
  )

  const isValidView =
    xColumn &&
    columnKeys.includes(xColumn) &&
    yColumn &&
    columnKeys.includes(yColumn)

  if (!isValidView) {
    return <EmptyGraphMessage message={INVALID_DATA_COPY} />
  }

  const colorHexes =
    colors && colors.length
      ? colors.map(c => c.hex)
      : DEFAULT_LINE_COLORS.map(c => c.hex)

  const interpolation = geomToInterpolation(geom)

  const groupKey = [...fluxGroupKeyUnion, 'result']

  const legendColumns = filterNoisyColumns(
    [...groupKey, xColumn, yColumn],
    table
  )

  const xFormatter = getFormatter(table.getColumnType(xColumn), {
    prefix: xTickPrefix,
    suffix: xTickSuffix,
    base: xTickBase,
    timeZone,
    timeFormat,
  })

  const yFormatter = getFormatter(table.getColumnType(yColumn), {
    prefix: yTickPrefix,
    suffix: yTickSuffix,
    base: yTickBase,
    timeZone,
    timeFormat,
  })

  const config: Config = {
    ...VIS_THEME,
    table,
    xAxisLabel,
    yAxisLabel,
    xDomain,
    onSetXDomain,
    onResetXDomain,
    yDomain,
    onSetYDomain,
    onResetYDomain,
    legendColumns,
    valueFormatters: {
      [xColumn]: xFormatter,
      [yColumn]: yFormatter,
    },
    layers: [
      {
        type: 'line',
        x: xColumn,
        y: yColumn,
        fill: groupKey,
        interpolation,
        position,
        colors: colorHexes,
        shadeBelow: !!shadeBelow,
        shadeBelowOpacity: 0.08,
      },
    ],
  }

  console.log('config: ', config)

  return (
    <>
      {loading === RemoteDataState.Loading && <GraphLoadingDots />}
      {children(config)}
    </>
  )
}

export default XYPlot
