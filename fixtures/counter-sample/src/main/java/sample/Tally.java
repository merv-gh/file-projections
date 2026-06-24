package sample;

public class Tally {
    // Counts processed items. Ops report the count comes out roughly double for
    // "rush" batches, but the loop body is hard to follow — somewhere the counter
    // is bumped more than once per item.
    public int run(int[] items, boolean rush) {
        int count = 0;
        int total = 0;
        for (int i = 0; i < items.length; i++) {
            count = count + 1;
            total = total + items[i];
            if (rush) {
                count = count + 1;
            }
        }
        return count;
    }
}
