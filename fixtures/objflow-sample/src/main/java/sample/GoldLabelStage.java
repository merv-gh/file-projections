package sample;

public class GoldLabelStage {
    public void apply(Receipt r, int amount) {
        int net = amount - amount / 20;
        r.setLabel("$" + net + "*");
    }
}
