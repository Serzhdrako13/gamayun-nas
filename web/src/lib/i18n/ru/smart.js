// lib/smart.js, lib/SmartChart.svelte: итог SMART и графики истории.
export default {
  disabledReason: 'SMART выключен — состояние диска не отслеживается',
  badge: { manualOff: 'выключен вручную', disabled: 'SMART выключен', notPolled: 'не опрошен', noSmart: 'нет SMART', warning: 'внимание', critical: 'неисправность', standby: 'спит', noData: 'нет данных' },
  sectors: { reallocated: 'переназначенные', pending: 'ожидающие', uncorrectable: 'нечитаемые' },
  chart: {
    history: 'История',
    period: 'Период',
    day: 'сутки',
    days: '{n} дн.',
    noPoints: 'Точек пока нет.',
    temp: 'Температура, °C',
    tempMax: ' · максимум {v}',
    tempNow: ' · сейчас {v}',
    tempAria: 'График температуры диска за {n} дн.',
    limits: ' · пороги {warn}/{crit}',
    badSectors: 'Плохие секторы',
    badSectorsZero: ': ноль за период',
    sectAria: 'График плохих секторов',
  },
};
