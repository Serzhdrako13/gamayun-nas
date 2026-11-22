// lib/smart.js, lib/SmartChart.svelte: SMART summary and history charts.
export default {
  disabledReason: 'SMART is off — disk health is not monitored',
  badge: { manualOff: 'off (manually)', disabled: 'SMART off', notPolled: 'not polled', noSmart: 'no SMART', warning: 'warning', critical: 'failing', standby: 'asleep', noData: 'no data' },
  sectors: { reallocated: 'reallocated', pending: 'pending', uncorrectable: 'uncorrectable' },
  chart: {
    history: 'History',
    period: 'Period',
    day: '24 h',
    days: '{n} d',
    noPoints: 'No data points yet.',
    temp: 'Temperature, °C',
    tempMax: ' · max {v}',
    tempNow: ' · now {v}',
    tempAria: 'Disk temperature chart for {n} d',
    limits: ' · thresholds {warn}/{crit}',
    badSectors: 'Bad sectors',
    badSectorsZero: ': none in this period',
    sectAria: 'Bad sectors chart',
  },
};
